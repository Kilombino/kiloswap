package policy

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/Kilombino/kiloswap/guard/internal/swapscript"
)

// The guard's own work, run every few seconds (Tick) and on the backend's
// request (Claim, Refund):
//   - payments: settle the record of a payment whose stream ended without
//     a final state by asking lnd; claim the lockup of every succeeded one
//     through the claim leaf, to an address of lnd's wallet;
//   - lockups: find the output; when the user claims through the claim
//     leaf, take the preimage from the witness and settle the hold invoice;
//     after the timeout, refund through the refund leaf to lnd's wallet;
//   - hold invoices: close the record of those lnd cancelled or settled.

// ErrNotYet is returned when a claim or refund is not possible yet.
var ErrNotYet = errors.New("not yet")

const dust = 330

// Tick does one round of the guard's own work.
func (p *Policy) Tick(ctx context.Context) {
	p.reconcilePayments(ctx)
	p.claimPaid(ctx)
	p.watchClaims(ctx)
	p.watchLockups(ctx)
	p.closeHolds(ctx)
}

func (p *Policy) reconcilePayments(ctx context.Context) {
	p.Ledger.Lock()
	var check []*Payment
	for _, x := range p.Ledger.Payments {
		if x.State == PayUnknown || (x.State == PayInflight && p.now().Sub(x.At).Minutes() > 2) {
			check = append(check, x)
		}
	}
	p.Ledger.Unlock()
	for _, x := range check {
		// asked even while its stream looks open: a stalled stream must not
		// hide a payment that succeeded (and must be claimed in time)
		h, _ := hex.DecodeString(x.Hash)
		status, pre, err := p.LN.TrackPayment(ctx, h)
		if err != nil {
			continue
		}
		p.Ledger.Lock()
		switch status {
		case paymentSucceeded:
			p.paid(x, pre)
		case paymentFailed:
			p.failed(x)
		case 0:
			// lnd never started it (refused before sending): give it back
			if x.State == PayUnknown && p.now().Sub(x.At).Minutes() > 10 {
				p.failed(x)
			}
		}
		p.Ledger.Unlock()
	}
}

func (p *Policy) claimPaid(ctx context.Context) {
	p.Ledger.Lock()
	var todo []*Payment
	for _, x := range p.Ledger.Payments {
		if x.State == PaySucceeded {
			todo = append(todo, x)
		}
	}
	p.Ledger.Unlock()
	for _, x := range todo {
		if _, _, err := p.claim(ctx, x); err != nil && !errors.Is(err, ErrNotYet) {
			p.pmu.Lock()
			if p.claimErr == nil {
				p.claimErr = map[string]string{}
			}
			repeat := p.claimErr[x.Hash] == err.Error()
			p.claimErr[x.Hash] = err.Error()
			p.pmu.Unlock()
			if !repeat {
				p.notify("guard_claim_failed", map[string]any{"swap": x.SwapID, "error": err.Error()})
			}
		}
	}
}

// Claim claims the lockup of the submarine swap with that id (the backend
// asks once its payment succeeded): the transaction id and its fee.
func (p *Policy) Claim(ctx context.Context, swapID string) (string, int64, error) {
	p.Ledger.Lock()
	var x *Payment
	for _, y := range p.Ledger.Payments {
		if y.SwapID == swapID {
			x = y
		}
	}
	p.Ledger.Unlock()
	if x == nil {
		return "", 0, fmt.Errorf("no payment of swap %s", swapID)
	}
	return p.claim(ctx, x)
}

func (p *Policy) claim(ctx context.Context, x *Payment) (string, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Ledger.Lock()
	state, claimTx, fee, pre := x.State, x.ClaimTx, x.ClaimFee, x.Preimage
	p.Ledger.Unlock()
	if claimTx != "" {
		return claimTx, fee, nil
	}
	if state != PaySucceeded {
		return "", 0, fmt.Errorf("%w: the payment is %s", ErrNotYet, state)
	}
	preimage, _ := hex.DecodeString(pre)
	hash, _ := hex.DecodeString(x.Hash)
	server, err := p.Keys.Private(x.KeyIndex)
	if err != nil {
		return "", 0, err
	}
	user, err := p.userKey(x.UserKey)
	if err != nil {
		return "", 0, err
	}
	out, err := swapscript.Build(swapscript.Params{Kind: swapscript.Submarine, Server: server.PubKey(), User: user, PaymentHash: hash, Timeout: x.Timeout})
	if err != nil {
		return "", 0, err
	}
	o, err := p.Chain.Output(ctx, x.LockupTx, x.Vout)
	if err != nil {
		return "", 0, err
	}
	if !out.Matches(o.PkScript) {
		return "", 0, errors.New("the lockup output changed")
	}
	height, _ := p.Chain.Height(ctx)
	sp := &spendFee{urgent: p.urgent(x, height)}
	tx, fee, err := p.spendLeafWith(ctx, out, out.ClaimLeaf, x.LockupTx, x.Vout, o, 0, func(sig, leaf, cb []byte) wire.TxWitness {
		return wire.TxWitness{sig, preimage, leaf, cb}
	}, server, sp)
	if err != nil {
		return "", 0, err
	}
	txid, err := p.broadcast(ctx, tx)
	if err != nil {
		return "", 0, err
	}
	p.Ledger.Lock()
	x.ClaimTx, x.ClaimFee, x.State = txid, fee, PayClaimed
	x.Bump = Bump{TxIDs: []string{txid}, Dest: sp.dest, Rate: sp.rate, Height: height}
	_ = p.Ledger.save()
	p.Ledger.Unlock()
	p.notify("claimed", map[string]any{"swap": x.SwapID, "sat": o.Sat - fee, "fee": fee, "txid": txid})
	return txid, fee, nil
}

// spendLeafWith builds and signs a transaction spending the swap output
// txid:vout through leaf to a fresh address of lnd's wallet. lockTime is
// the refund leaf's timeout (0 for a claim).
//
// sp carries a replacement: its address (kept, so a replacement pays the
// same address) and the least fee rate it must pay (0 for a first spend).
// The address and fee rate used are written back to sp.
func (p *Policy) spendLeafWith(ctx context.Context, out *swapscript.Output, leaf txscript.TapLeaf, txid string, vout uint32,
	o *Output, lockTime uint32, witness func(sig, leaf, cb []byte) wire.TxWitness, key *btcec.PrivateKey, sp *spendFee) (*wire.MsgTx, int64, error) {
	h, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return nil, 0, err
	}
	addr := sp.dest
	if addr == "" {
		if addr, err = p.LN.NewAddress(ctx); err != nil {
			return nil, 0, fmt.Errorf("lnd address: %w", err)
		}
	}
	pk, err := p.addrScript(addr)
	if err != nil {
		return nil, 0, fmt.Errorf("lnd address %s: %w", addr, err)
	}
	cb, err := out.ControlBlock(leaf)
	if err != nil {
		return nil, 0, err
	}
	tx := wire.NewMsgTx(2)
	in := wire.NewTxIn(wire.NewOutPoint(h, vout), nil, nil)
	in.Sequence = wire.MaxTxInSequenceNum - 2 // replaceable, and not final for the lock time
	tx.LockTime = lockTime
	tx.AddTxIn(in)
	tx.AddTxOut(wire.NewTxOut(o.Sat, pk))
	sign := func() error {
		fetcher := txscript.NewCannedPrevOutputFetcher(o.PkScript, o.Sat)
		sh := txscript.NewTxSigHashes(tx, fetcher)
		sig, err := txscript.RawTxInTapscriptSignature(tx, sh, 0, o.Sat, o.PkScript, leaf, txscript.SigHashDefault, key)
		if err != nil {
			return err
		}
		tx.TxIn[0].Witness = witness(sig, leaf.Script, cb)
		return nil
	}
	if err := sign(); err != nil {
		return nil, 0, err
	}
	weight := int64(tx.SerializeSizeStripped()*3 + tx.SerializeSize())
	vsize := (weight + 3) / 4
	rate, err := p.Chain.FeeRate(ctx)
	if err != nil || rate < 1 {
		rate = 1
	}
	if rate < sp.minRate {
		rate = sp.minRate
	}
	maxRate := float64(p.MaxFeeRate)
	if sp.urgent && p.UrgentFeeFraction > 0 {
		// close to the timeout a claim must confirm whatever it costs
		// (at least MAX_FEE_RATE, doubling with each replacement)
		maxRate = math.Max(maxRate, float64(o.Sat)*p.UrgentFeeFraction/float64(vsize))
		rate = math.Max(rate, math.Max(float64(p.MaxFeeRate), sp.minRate*2))
	}
	if rate > maxRate {
		rate = maxRate
	}
	fee := int64(math.Ceil(float64(vsize) * rate))
	if o.Sat-fee < dust {
		return nil, 0, fmt.Errorf("%d sat do not cover a %d sat fee", o.Sat, fee)
	}
	tx.TxOut[0].Value = o.Sat - fee
	if err := sign(); err != nil {
		return nil, 0, err
	}
	sp.dest, sp.rate = addr, rate
	return tx, fee, nil
}

// spendFee is a guard spend's address and fee rate (see spendLeafWith).
type spendFee struct {
	dest    string
	minRate float64
	rate    float64
	urgent  bool // a claim close to its timeout: past MaxFeeRate if needed
}

func (p *Policy) broadcast(ctx context.Context, tx *wire.MsgTx) (string, error) {
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		return "", err
	}
	return p.Chain.Broadcast(ctx, hex.EncodeToString(b.Bytes()))
}

func (p *Policy) watchLockups(ctx context.Context) {
	p.Ledger.Lock()
	var todo []*Lockup
	var approved []*Lockup
	for _, l := range p.Ledger.Lockups {
		switch l.State {
		case LockSent, LockClaimed, LockRefunding:
			todo = append(todo, l)
		case LockApproved:
			if p.now().Sub(l.At) > 2*time.Minute {
				approved = append(approved, l)
			}
		}
	}
	p.Ledger.Unlock()
	height, err := p.Chain.Height(ctx)
	if err != nil {
		return
	}
	for _, l := range approved {
		// lnd's answer never reached the guard (a stalled stream): look for
		// the lockup on chain by its rebuilt output and amount
		if p.findApproved(ctx, l, height) {
			todo = append(todo, l)
		}
	}
	for _, l := range todo {
		if err := p.watchLockup(ctx, l, height); err != nil && !errors.Is(err, ErrNotYet) {
			p.Ledger.Lock()
			repeat := l.Note == err.Error()
			l.Note = err.Error()
			_ = p.Ledger.save()
			p.Ledger.Unlock()
			if !repeat { // once per new problem, not every round
				p.notify("guard_lockup_error", map[string]any{"swap": l.SwapID, "error": err.Error()})
			}
		}
	}
}

// findApproved looks for an allowed lockup whose send lnd never confirmed
// to the guard, among lnd's own wallet transactions (a payment to the same
// address by anyone else is no lockup of ours); once found and confirmed it
// is followed like any other (claims settle, the timeout refunds). One never
// found after ApprovedExpireBlocks is given up.
func (p *Policy) findApproved(ctx context.Context, l *Lockup, height int64) bool {
	txid, vout, confs, err := p.LN.FindWalletSend(ctx, l.Address, l.Sat, l.SentAt-1)
	if err != nil {
		return false
	}
	if txid == "" {
		if p.ApprovedExpireBlocks > 0 && height-l.SentAt > p.ApprovedExpireBlocks {
			p.Ledger.Lock()
			if l.State == LockApproved {
				l.State = LockExpired
				delete(p.Ledger.Entries, "send:"+l.Hash)
				_ = p.Ledger.save()
			}
			p.Ledger.Unlock()
			p.notify("guard_lockup_expired", map[string]any{"swap": l.SwapID, "sat": l.Sat})
		}
		return false
	}
	if confs < 1 {
		return false // in lnd's wallet but not in a block yet: next round
	}
	p.Ledger.Lock()
	if l.State == LockApproved {
		l.State, l.TxID, l.Vout = LockSent, txid, int32(vout)
		_ = p.Ledger.save()
	}
	p.Ledger.Unlock()
	p.notify("lockup_sent", map[string]any{"swap": l.SwapID, "sat": l.Sat, "txid": txid, "found": true})
	return true
}

func (p *Policy) lockupOutput(ctx context.Context, l *Lockup) (*swapscript.Output, *Output, uint32, error) {
	hash, _ := hex.DecodeString(l.Hash)
	server, err := p.Keys.Public(l.KeyIndex)
	if err != nil {
		return nil, nil, 0, err
	}
	user, err := p.userKey(l.UserKey)
	if err != nil {
		return nil, nil, 0, err
	}
	out, err := swapscript.Build(swapscript.Params{Kind: swapscript.Reverse, Server: server, User: user, PaymentHash: hash, Timeout: l.Timeout})
	if err != nil {
		return nil, nil, 0, err
	}
	if l.Vout < 0 {
		outs, err := p.Chain.Outputs(ctx, l.TxID)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("%w: lockup %s not seen: %v", ErrNotYet, l.TxID, err)
		}
		for i, o := range outs {
			if out.Matches(o.PkScript) && o.Sat == l.Sat {
				p.Ledger.Lock()
				l.Vout = int32(i)
				_ = p.Ledger.save()
				p.Ledger.Unlock()
				break
			}
		}
		if l.Vout < 0 {
			return nil, nil, 0, fmt.Errorf("lockup %s pays no %d sat to the swap output", l.TxID, l.Sat)
		}
	}
	o, err := p.Chain.Output(ctx, l.TxID, uint32(l.Vout))
	if err != nil {
		return nil, nil, 0, err
	}
	return out, o, uint32(l.Vout), nil
}

func (p *Policy) watchLockup(ctx context.Context, l *Lockup, height int64) error {
	out, o, vout, err := p.lockupOutput(ctx, l)
	if errors.Is(err, ErrNotYet) && height-l.SentAt > bumpEvery*2 {
		// the transaction lnd named is not on chain (replaced?): look in
		// lnd's wallet for what paid the lockup instead
		if txid, v, confs, ferr := p.LN.FindWalletSend(ctx, l.Address, l.Sat, l.SentAt-1); ferr == nil && txid != "" && confs >= 1 && !strings.EqualFold(txid, l.TxID) {
			p.Ledger.Lock()
			l.TxID, l.Vout = txid, int32(v)
			_ = p.Ledger.save()
			p.Ledger.Unlock()
			out, o, vout, err = p.lockupOutput(ctx, l)
		}
	}
	if err != nil {
		return err
	}
	unspent, err := p.Chain.Unspent(ctx, l.TxID, vout)
	if err != nil {
		return err
	}
	if !unspent {
		sp, err := p.Chain.Spender(ctx, l.TxID, vout, l.SentAt)
		if err != nil {
			return err
		}
		if sp == nil {
			return fmt.Errorf("%w: spender of %s:%d not found yet", ErrNotYet, l.TxID, vout)
		}
		hash, _ := hex.DecodeString(l.Hash)
		if pre, ok := out.PreimageFromClaim(sp.Witness, hash); ok {
			return p.settleClaimed(ctx, l, pre, sp.TxID)
		}
		if l.State == LockRefunding && (strings.EqualFold(sp.TxID, l.RefundTx) || l.Bump.Ours(sp.TxID)) {
			if sp.Confirmations < 1 {
				return p.bumpRefund(ctx, l, out, o, vout, height, true)
			}
			if sp.Confirmations >= finalConfs {
				p.Ledger.Lock()
				l.State = LockRefunded
				_ = p.Ledger.save()
				p.Ledger.Unlock()
				p.notify("refund_confirmed", map[string]any{"swap": l.SwapID, "txid": l.RefundTx})
				// the coins are back: the payer's HTLCs go back too
				hash, _ := hex.DecodeString(l.Hash)
				if err := p.LN.CancelInvoice(ctx, hash); err == nil {
					p.Ledger.Lock()
					if h := p.Ledger.Holds[l.Hash]; h != nil {
						h.State = "canceled"
					}
					_ = p.Ledger.save()
					p.Ledger.Unlock()
				}
			}
			return nil
		}
		return fmt.Errorf("lockup %s:%d spent by %s, neither a claim nor our refund", l.TxID, vout, sp.TxID)
	}
	if l.State == LockSent && height >= int64(l.Timeout) {
		_, _, err := p.refund(ctx, l, out, o, vout)
		return err
	}
	if l.State == LockRefunding {
		// our refund is in no block and not in the mempool: dropped
		return p.bumpRefund(ctx, l, out, o, vout, height, false)
	}
	return nil
}

func (p *Policy) settleClaimed(ctx context.Context, l *Lockup, pre []byte, spender string) error {
	p.Ledger.Lock()
	if l.State == LockSettled {
		p.Ledger.Unlock()
		return nil
	}
	l.State, l.Preimage = LockClaimed, hex.EncodeToString(pre)
	_ = p.Ledger.save()
	p.Ledger.Unlock()
	if err := p.LN.SettleInvoice(ctx, pre); err != nil {
		// already settled by the backend is fine: check lnd's own record
		hash, _ := hex.DecodeString(l.Hash)
		inv, lerr := p.LN.LookupInvoice(ctx, hash)
		if lerr != nil || inv == nil || inv.State != InvoiceSettled {
			return fmt.Errorf("settle after the claim %s: %v", spender, err)
		}
	}
	p.Ledger.Lock()
	l.State = LockSettled
	if h := p.Ledger.Holds[l.Hash]; h != nil {
		h.State = "settled"
	}
	_ = p.Ledger.save()
	p.Ledger.Unlock()
	p.notify("settled", map[string]any{"swap": l.SwapID, "sat": l.Sat, "claim": spender})
	return nil
}

// Refund refunds the lockup of the reverse swap with that id once its
// timeout has passed: the transaction id and its fee.
func (p *Policy) Refund(ctx context.Context, swapID string) (string, int64, error) {
	p.Ledger.Lock()
	var l *Lockup
	for _, y := range p.Ledger.Lockups {
		if y.SwapID == swapID {
			l = y
		}
	}
	p.Ledger.Unlock()
	if l == nil {
		return "", 0, fmt.Errorf("no lockup of swap %s", swapID)
	}
	if l.RefundTx != "" {
		return l.RefundTx, 0, nil
	}
	height, err := p.Chain.Height(ctx)
	if err != nil {
		return "", 0, err
	}
	if height < int64(l.Timeout) {
		return "", 0, fmt.Errorf("%w: timeout at %d, now %d", ErrNotYet, l.Timeout, height)
	}
	out, o, vout, err := p.lockupOutput(ctx, l)
	if err != nil {
		return "", 0, err
	}
	return p.refund(ctx, l, out, o, vout)
}

func (p *Policy) refund(ctx context.Context, l *Lockup, out *swapscript.Output, o *Output, vout uint32) (string, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.RefundTx != "" {
		return l.RefundTx, 0, nil
	}
	server, err := p.Keys.Private(l.KeyIndex)
	if err != nil {
		return "", 0, err
	}
	sp := &spendFee{}
	tx, fee, err := p.spendLeafWith(ctx, out, out.RefundLeaf, l.TxID, vout, o, l.Timeout, func(sig, leaf, cb []byte) wire.TxWitness {
		return wire.TxWitness{sig, leaf, cb}
	}, server, sp)
	if err != nil {
		return "", 0, err
	}
	txid, err := p.broadcast(ctx, tx)
	if err != nil {
		return "", 0, err
	}
	height, _ := p.Chain.Height(ctx)
	p.Ledger.Lock()
	l.State, l.RefundTx = LockRefunding, txid
	l.Bump = Bump{TxIDs: []string{txid}, Dest: sp.dest, Rate: sp.rate, Height: height}
	_ = p.Ledger.save()
	p.Ledger.Unlock()
	p.notify("refunded", map[string]any{"swap": l.SwapID, "sat": o.Sat - fee, "fee": fee, "txid": txid})
	return txid, fee, nil
}

// finalConfs is how deep one of the guard's own spends must be before it is
// taken as done (a reorg can undo a shallow one; until then it is watched
// and broadcast again if it falls out).
const finalConfs = 3

// bumpEvery is how many blocks one of the guard's own spends may stay
// unconfirmed before the guard replaces it with a higher fee.
const bumpEvery = 3

// nextRate is the least fee rate of a replacement: half as much again plus
// 1 sat/vB, which also covers BIP 125's increment over the replaced fee.
func nextRate(last float64) float64 { return last*1.5 + 1 }

// urgent tells whether the claim of x is close enough to the swap's timeout
// to pay past MaxFeeRate.
func (p *Policy) urgent(x *Payment, height int64) bool {
	return p.UrgentBlocks > 0 && int64(x.Timeout)-height <= p.UrgentBlocks
}

// replacementRate is the least fee rate for replacing (or broadcasting
// again) a guard spend last broadcast as b, and whether to do it now: one
// still in the mempool after bumpEvery blocks gets a higher fee (none past
// max, where it waits); one that fell out of it goes again at once, at a
// higher fee too once bumpEvery blocks have passed (the mempool dropped it
// for its fee, most likely).
func replacementRate(b Bump, height int64, inMempool bool, max float64) (float64, bool) {
	waited := height-b.Height >= bumpEvery
	switch {
	case inMempool && (!waited || b.Rate >= max):
		return 0, false
	case waited:
		return nextRate(b.Rate), true
	default:
		return b.Rate, true
	}
}

// bumpRefund replaces our refund of l when it stayed unconfirmed bumpEvery
// blocks (inMempool) or fell out of the mempool (rebroadcast at once, at
// least at the last fee rate).
func (p *Policy) bumpRefund(ctx context.Context, l *Lockup, out *swapscript.Output, o *Output, vout uint32, height int64, inMempool bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Ledger.Lock()
	b := l.Bump
	p.Ledger.Unlock()
	if b.Dest == "" {
		return nil // a refund from before bumps were kept: nothing to replace it with
	}
	min, ok := replacementRate(b, height, inMempool, float64(p.MaxFeeRate))
	if !ok {
		return nil
	}
	server, err := p.Keys.Private(l.KeyIndex)
	if err != nil {
		return err
	}
	sp := &spendFee{dest: b.Dest, minRate: min}
	tx, fee, err := p.spendLeafWith(ctx, out, out.RefundLeaf, l.TxID, vout, o, l.Timeout, func(sig, leaf, cb []byte) wire.TxWitness {
		return wire.TxWitness{sig, leaf, cb}
	}, server, sp)
	if err != nil {
		return err
	}
	txid, err := p.broadcast(ctx, tx)
	if err != nil {
		return fmt.Errorf("refund replacement: %w", err)
	}
	p.Ledger.Lock()
	l.RefundTx = txid
	if !l.Bump.Ours(txid) {
		l.Bump.TxIDs = append(l.Bump.TxIDs, txid)
	}
	l.Bump.Rate, l.Bump.Height = sp.rate, height
	_ = p.Ledger.save()
	p.Ledger.Unlock()
	p.notify("refund_bumped", map[string]any{"swap": l.SwapID, "fee": fee, "rate": sp.rate, "txid": txid})
	return nil
}

// watchClaims follows the guard's claims of submarine lockups until one
// confirms: a claim that stays unconfirmed bumpEvery blocks is replaced with
// a higher fee, one that fell out of the mempool is broadcast again. Before
// the swap's timeout the user cannot refund, so it must confirm by then.
func (p *Policy) watchClaims(ctx context.Context) {
	p.Ledger.Lock()
	var todo []*Payment
	for _, x := range p.Ledger.Payments {
		if x.State == PayClaimed && !x.Bump.Done && x.Bump.Dest != "" {
			todo = append(todo, x)
		}
	}
	p.Ledger.Unlock()
	if len(todo) == 0 {
		return
	}
	height, err := p.Chain.Height(ctx)
	if err != nil {
		return
	}
	for _, x := range todo {
		if err := p.watchClaim(ctx, x, height); err != nil {
			p.notify("guard_claim_failed", map[string]any{"swap": x.SwapID, "error": err.Error()})
		}
	}
}

func (p *Policy) watchClaim(ctx context.Context, x *Payment, height int64) error {
	unspent, err := p.Chain.Unspent(ctx, x.LockupTx, x.Vout)
	if err != nil {
		return nil
	}
	inMempool := false
	if !unspent {
		sp, err := p.Chain.Spender(ctx, x.LockupTx, x.Vout, x.Bump.Height-1)
		if err != nil || sp == nil {
			return nil // not found yet
		}
		if !x.Bump.Ours(sp.TxID) {
			p.Ledger.Lock()
			x.Bump.Done = true
			_ = p.Ledger.save()
			p.Ledger.Unlock()
			return fmt.Errorf("lockup %s:%d spent by %s, not our claim", x.LockupTx, x.Vout, sp.TxID)
		}
		if sp.Confirmations >= finalConfs {
			p.Ledger.Lock()
			x.Bump.Done, x.ClaimTx = true, sp.TxID
			_ = p.Ledger.save()
			p.Ledger.Unlock()
			p.notify("claim_confirmed", map[string]any{"swap": x.SwapID, "txid": sp.TxID})
			return nil
		}
		if sp.Confirmations >= 1 {
			return nil // in a block: wait until it is deep enough
		}
		inMempool = true
	}
	return p.bumpClaim(ctx, x, height, inMempool)
}

func (p *Policy) bumpClaim(ctx context.Context, x *Payment, height int64, inMempool bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Ledger.Lock()
	b, pre := x.Bump, x.Preimage
	p.Ledger.Unlock()
	limit := float64(p.MaxFeeRate)
	if p.urgent(x, height) {
		limit = math.MaxFloat64 // bounded by UrgentFeeFraction when built
		if inMempool && height > b.Height {
			b.Height = height - bumpEvery // near the timeout: replace every block
		}
	}
	min, ok := replacementRate(b, height, inMempool, limit)
	if !ok {
		return nil
	}
	preimage, _ := hex.DecodeString(pre)
	hash, _ := hex.DecodeString(x.Hash)
	server, err := p.Keys.Private(x.KeyIndex)
	if err != nil {
		return err
	}
	user, err := p.userKey(x.UserKey)
	if err != nil {
		return err
	}
	out, err := swapscript.Build(swapscript.Params{Kind: swapscript.Submarine, Server: server.PubKey(), User: user, PaymentHash: hash, Timeout: x.Timeout})
	if err != nil {
		return err
	}
	o, err := p.Chain.Output(ctx, x.LockupTx, x.Vout)
	if err != nil {
		return err
	}
	if !out.Matches(o.PkScript) {
		return errors.New("the lockup output changed")
	}
	sp := &spendFee{dest: b.Dest, minRate: min, urgent: p.urgent(x, height)}
	tx, fee, err := p.spendLeafWith(ctx, out, out.ClaimLeaf, x.LockupTx, x.Vout, o, 0, func(sig, leaf, cb []byte) wire.TxWitness {
		return wire.TxWitness{sig, preimage, leaf, cb}
	}, server, sp)
	if err != nil {
		return err
	}
	txid, err := p.broadcast(ctx, tx)
	if err != nil {
		return fmt.Errorf("claim replacement: %w", err)
	}
	p.Ledger.Lock()
	x.ClaimTx, x.ClaimFee = txid, fee
	if !x.Bump.Ours(txid) {
		x.Bump.TxIDs = append(x.Bump.TxIDs, txid)
	}
	x.Bump.Rate, x.Bump.Height = sp.rate, height
	_ = p.Ledger.save()
	p.Ledger.Unlock()
	p.notify("claim_bumped", map[string]any{"swap": x.SwapID, "fee": fee, "rate": sp.rate, "txid": txid})
	return nil
}

func (p *Policy) closeHolds(ctx context.Context) {
	p.Ledger.Lock()
	var open []string
	for h, x := range p.Ledger.Holds {
		if x.State == "open" || x.State == HoldCanceling {
			open = append(open, h)
		}
	}
	p.Ledger.Unlock()
	for _, h := range open {
		hash, _ := hex.DecodeString(h)
		inv, err := p.LN.LookupInvoice(ctx, hash)
		if err != nil {
			continue
		}
		state := ""
		switch {
		case inv == nil, inv.State == InvoiceCanceled:
			state = "canceled"
		case inv.State == InvoiceSettled:
			state = "settled"
		}
		if state == "" {
			// a cancel allowed whose answer never came: open again unless
			// one is still waiting
			p.Ledger.Lock()
			if x := p.Ledger.Holds[h]; x != nil && x.State == HoldCanceling && !p.cancelPending(h) {
				x.State = "open"
				_ = p.Ledger.save()
			}
			p.Ledger.Unlock()
			continue
		}
		p.Ledger.Lock()
		if x := p.Ledger.Holds[h]; x != nil && (x.State == "open" || x.State == HoldCanceling) {
			x.State = state
			_ = p.Ledger.save()
		}
		p.Ledger.Unlock()
	}
}

func (p *Policy) cancelPending(hash string) bool {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for _, r := range p.pending {
		if r.method == "/invoicesrpc.Invoices/CancelInvoice" && r.hash == hash {
			return true
		}
	}
	return false
}
