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
	_ = p.Ledger.SaveIfDirty()
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
			// lnd has no such payment (never started, or refused before
			// sending): after ten minutes it is given back, whether the
			// stream said nothing or looks still open
			if p.now().Sub(x.At).Minutes() > 10 {
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
	p.smu.Lock()
	defer p.smu.Unlock()
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
	// a claim broadcast before a crash, not recorded: only our key can spend
	// the claim leaf, so a claim-leaf spend is ours
	if unspent, err := p.Chain.Unspent(ctx, x.LockupTx, x.Vout); err == nil && !unspent {
		if sp, err := p.Chain.Spender(ctx, x.LockupTx, x.Vout, height-maxScanBack); err == nil && sp != nil && out.SpendsLeaf(sp.Witness, out.ClaimLeaf) {
			p.Ledger.Lock()
			x.ClaimTx, x.State = sp.TxID, PayClaimed
			x.Bump = Bump{TxIDs: []string{sp.TxID}, Dest: x.Bump.Dest, Height: height, Rate: 1}
			_ = p.Ledger.save()
			p.Ledger.Unlock()
			p.notify("claimed", map[string]any{"swap": x.SwapID, "txid": sp.TxID, "recovered": true})
			return sp.TxID, 0, nil
		}
	}
	// the same address on every attempt (a failed broadcast must not burn
	// a new address of lnd's each round)
	sp := &spendFee{urgent: p.urgent(x, height), dest: x.Bump.Dest}
	tx, fee, err := p.spendLeafWith(ctx, out, out.ClaimLeaf, x.LockupTx, x.Vout, o, 0, func(sig, leaf, cb []byte) wire.TxWitness {
		return wire.TxWitness{sig, preimage, leaf, cb}
	}, server, sp)
	if err != nil {
		return "", 0, err
	}
	p.Ledger.Lock()
	x.Bump.Dest = sp.dest
	_ = p.Ledger.save()
	p.Ledger.Unlock()
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
		case LockSettled:
			// paid but never claimed on chain: refunded after a long grace
			if l.TxID != "" {
				todo = append(todo, l)
			}
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
	// the lockups on chain first: settling, refunding and claiming must
	// never wait behind the search for approvals lnd did not confirm
	defer p.searchApproved(ctx, approved, height)
	for _, l := range todo {
		err := p.watchLockup(ctx, l, height)
		// a lockup not confirmed, or a step that keeps saying "not yet",
		// for more than six blocks is the operator's to look at
		p.Ledger.Lock()
		state, txid := l.State, l.TxID
		p.Ledger.Unlock()
		stuck := errors.Is(err, ErrNotYet) || (state == LockSent && txid != "" && p.unconfirmed(ctx, txid))
		p.Ledger.Lock()
		switch {
		case !stuck:
			l.StuckSince = 0
		case l.StuckSince == 0:
			l.StuckSince = height
		case height-l.StuckSince > 6 && !l.StuckAlerted:
			l.StuckAlerted = true
			p.notify("guard_lockup_stuck", map[string]any{"swap": l.SwapID, "blocks": height - l.StuckSince})
		}
		p.Ledger.Unlock()
		if err != nil && !errors.Is(err, ErrNotYet) {
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

// searchApproved looks for the lockups allowed whose send lnd never
// confirmed to the guard, within a budget of its own (after the real
// lockups' work), each from where its last search stopped.
func (p *Policy) searchApproved(ctx context.Context, approved []*Lockup, height int64) {
	ctx, cancel := context.WithTimeout(ctx, ApprovedBudget)
	defer cancel()
	for _, l := range approved {
		if ctx.Err() != nil {
			return
		}
		if p.findApproved(ctx, l, height) {
			_ = p.watchLockup(ctx, l, height)
		}
		p.Ledger.Lock()
		if l.State == LockApproved && height-l.SentAt > 6 && !l.StuckAlerted {
			l.StuckAlerted = true
			p.notify("guard_lockup_stuck", map[string]any{"swap": l.SwapID, "blocks": height - l.SentAt, "approved": true})
		}
		p.Ledger.Unlock()
	}
}

// ApprovedBudget is what one round may spend on approvals; ScanStep how
// many blocks one approval's chain search reads per round.
const (
	ApprovedBudget = 30 * time.Second
	ScanStep       = 144
)

// findApproved looks for an allowed lockup whose send lnd never confirmed
// to the guard. Only positive evidence moves it: an output paying its
// rebuilt script and amount in a transaction lnd's own wallet sent (found
// on chain block by block, or in lnd's wallet history, unconfirmed too).
// Outputs to the script that lnd's wallet did not send are decoys and count
// for nothing.
//
// An approval is given up only on complete proof that nothing of ours is at
// stake: lnd released the HTLCs, the timeout is long past, lnd's wallet
// history (read in full) shows no payment to the script, and the chain was
// scanned from the approval up to the tip without one lnd-owned output.
// Until then its reserve, daily share and the block on cancelling stay.
func (p *Policy) findApproved(ctx context.Context, l *Lockup, height int64) bool {
	adopt := func(txid string, vout uint32) bool {
		p.Ledger.Lock()
		if l.State == LockApproved {
			l.State, l.TxID, l.Vout = LockSent, txid, int32(vout)
			_ = p.Ledger.save()
		}
		p.Ledger.Unlock()
		p.notify("lockup_sent", map[string]any{"swap": l.SwapID, "sat": l.Sat, "txid": txid, "found": true})
		return true
	}
	pk, err := p.addrScript(l.Address)
	if err != nil {
		return false
	}
	if txid, vout, ok := p.scanOwn(ctx, l, pk, height); ok {
		return adopt(txid, vout)
	}
	txid, vout, _, found, werr := p.LN.WalletPaid(ctx, pk, l.Sat, l.SentAt-1)
	if werr == nil && found {
		return adopt(txid, vout)
	}
	if werr != nil || p.ApprovedExpireBlocks <= 0 || height-l.SentAt <= p.ApprovedExpireBlocks ||
		height <= int64(l.Timeout)+p.ReverseMarginBlocks+p.HoldExpiryDelta {
		return false
	}
	p.Ledger.Lock()
	scanned := l.ScanFrom
	p.Ledger.Unlock()
	if scanned <= height {
		return false // the chain is not scanned up to the tip yet
	}
	hash, _ := hex.DecodeString(l.Hash)
	inv, err := p.LN.LookupInvoice(ctx, hash)
	if err != nil || !(inv == nil || inv.State == InvoiceCanceled || inv.State == InvoiceSettled) {
		return false // lnd still holds (or may hold) the HTLCs
	}
	p.Ledger.Lock()
	if l.State == LockApproved {
		l.State = LockExpired
		delete(p.Ledger.Entries, "send:"+l.Hash)
		_ = p.Ledger.save()
	}
	p.Ledger.Unlock()
	p.notify("guard_lockup_expired", map[string]any{"swap": l.SwapID, "sat": l.Sat})
	return false
}

// scanOwn reads the next ScanStep blocks of l's chain search (from where it
// stopped, from the approval on, never skipping any) for an output paying
// its script and amount that lnd's wallet sent. l.ScanFrom is the next
// height to read; past the tip, the scan is complete (new blocks extend it).
func (p *Policy) scanOwn(ctx context.Context, l *Lockup, pk []byte, height int64) (string, uint32, bool) {
	p.Ledger.Lock()
	from := l.ScanFrom
	if from < l.SentAt-1 {
		from = l.SentAt - 1
	}
	p.Ledger.Unlock()
	if from > height {
		from = max(height-1, l.SentAt-1) // keep an eye on the newest blocks
	}
	to := min(height, from+ScanStep-1)
	found, err := p.Chain.FindOutputs(ctx, pk, l.Sat, from, to)
	if err != nil {
		return "", 0, false
	}
	for _, f := range found {
		if own, err := p.LN.WalletOwns(ctx, f.TxID, f.Height); err == nil && own {
			return f.TxID, f.Vout, true
		}
	}
	p.Ledger.Lock()
	if to+1 > l.ScanFrom {
		l.ScanFrom = to + 1
	}
	p.Ledger.Unlock()
	return "", 0, false
}

// findOwnLockup finds l's lockup sent by lnd's wallet anywhere since the
// approval (a full search, for a lockup whose named transaction vanished).
func (p *Policy) findOwnLockup(ctx context.Context, l *Lockup, height int64) (string, uint32, bool) {
	pk, err := p.addrScript(l.Address)
	if err != nil {
		return "", 0, false
	}
	found, err := p.Chain.FindOutputs(ctx, pk, l.Sat, l.SentAt-1, height)
	if err != nil {
		return "", 0, false
	}
	for _, f := range found {
		if own, err := p.LN.WalletOwns(ctx, f.TxID, f.Height); err == nil && own {
			return f.TxID, f.Vout, true
		}
	}
	return "", 0, false
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
		// the transaction lnd named is not on chain (replaced?): look on
		// chain for what lnd's wallet paid the lockup with instead
		if txid, v, ok := p.findOwnLockup(ctx, l, height); ok && !strings.EqualFold(txid, l.TxID) {
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
		if (l.State == LockSent || l.State == LockRefunding) && !l.Bump.Ours(sp.TxID) && !strings.EqualFold(sp.TxID, l.RefundTx) &&
			out.SpendsLeaf(sp.Witness, out.RefundLeaf) {
			// our refund, broadcast before a crash and not recorded: only our
			// key can spend the refund leaf
			p.Ledger.Lock()
			l.State, l.RefundTx = LockRefunding, sp.TxID
			l.Bump.TxIDs = append(l.Bump.TxIDs, sp.TxID)
			_ = p.Ledger.save()
			p.Ledger.Unlock()
		}
		if l.State == LockRefunding && (strings.EqualFold(sp.TxID, l.RefundTx) || l.Bump.Ours(sp.TxID)) {
			if sp.Confirmations < 1 {
				return p.bumpRefund(ctx, l, out, o, vout, height, true)
			}
			// the hold invoice is cancelled only when the refund is as deep as
			// the amount asks (a reorg that undid it would let the user claim
			// with the preimage after getting the payment back)
			if sp.Confirmations >= max(finalConfs, p.confsFor(p.refundExposure(ctx, l))) {
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
	if l.State == LockSettled && height >= int64(l.Timeout)+settledGrace {
		// the hold invoice was settled yet the user never claimed: the coins
		// would stay locked for good
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
	if l == nil {
		p.Ledger.Unlock()
		return "", 0, fmt.Errorf("no lockup of swap %s", swapID)
	}
	refundTx, state, timeout := l.RefundTx, l.State, l.Timeout
	p.Ledger.Unlock()
	if refundTx != "" {
		return refundTx, 0, nil
	}
	if state != LockSent && state != LockSettled {
		return "", 0, fmt.Errorf("%w: the lockup is %s", ErrNotYet, state)
	}
	if state == LockSettled {
		timeout += settledGrace
	}
	height, err := p.Chain.Height(ctx)
	if err != nil {
		return "", 0, err
	}
	if height < int64(timeout) {
		return "", 0, fmt.Errorf("%w: timeout at %d, now %d", ErrNotYet, timeout, height)
	}
	out, o, vout, err := p.lockupOutput(ctx, l)
	if err != nil {
		return "", 0, err
	}
	return p.refund(ctx, l, out, o, vout)
}

func (p *Policy) refund(ctx context.Context, l *Lockup, out *swapscript.Output, o *Output, vout uint32) (string, int64, error) {
	p.smu.Lock()
	defer p.smu.Unlock()
	p.Ledger.Lock()
	refundTx, dest := l.RefundTx, l.Bump.Dest
	p.Ledger.Unlock()
	if refundTx != "" {
		return refundTx, 0, nil
	}
	server, err := p.Keys.Private(l.KeyIndex)
	if err != nil {
		return "", 0, err
	}
	sp := &spendFee{dest: dest} // the same address on every attempt
	tx, fee, err := p.spendLeafWith(ctx, out, out.RefundLeaf, l.TxID, vout, o, l.Timeout, func(sig, leaf, cb []byte) wire.TxWitness {
		return wire.TxWitness{sig, leaf, cb}
	}, server, sp)
	if err != nil {
		return "", 0, err
	}
	p.Ledger.Lock()
	l.Bump.Dest = sp.dest
	p.saveOrAlert()
	p.Ledger.Unlock()
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
	p.smu.Lock()
	defer p.smu.Unlock()
	p.Ledger.Lock()
	b := l.Bump
	p.Ledger.Unlock()
	if b.Dest == "" {
		return nil // a refund from before bumps were kept: nothing to replace it with
	}
	// the hold invoice's HTLCs expire some blocks after the timeout: past
	// half that margin the refund pays what it takes, every block
	urgent := p.ReverseMarginBlocks > 0 && height-int64(l.Timeout) >= p.ReverseMarginBlocks/2
	limit := float64(p.MaxFeeRate)
	if urgent {
		limit = math.MaxFloat64
		if inMempool && height > b.Height {
			b.Height = height - bumpEvery
		}
	}
	min, ok := replacementRate(b, height, inMempool, limit)
	if !ok {
		return nil
	}
	server, err := p.Keys.Private(l.KeyIndex)
	if err != nil {
		return err
	}
	sp := &spendFee{dest: b.Dest, minRate: min, urgent: urgent}
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
		if !x.Bump.Ours(sp.TxID) && p.claimLeafSpend(x, sp) {
			p.Ledger.Lock()
			x.Bump.TxIDs = append(x.Bump.TxIDs, sp.TxID) // ours, from before a crash
			x.ClaimTx = sp.TxID
			_ = p.Ledger.save()
			p.Ledger.Unlock()
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
	p.smu.Lock()
	defer p.smu.Unlock()
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
	var todo []string
	for h, x := range p.Ledger.Holds {
		if x.State == "open" || x.State == HoldCanceling || x.State == HoldCreating {
			todo = append(todo, h)
		}
	}
	p.Ledger.Unlock()
	for _, h := range todo {
		p.closeHold(ctx, h)
	}
}

// closeHold brings one hold's record in line with lnd. Changes that could
// let a lockup through (back to open) or stop one (cancelling) are made
// under the judgment lock, after a fresh look at lnd.
func (p *Policy) closeHold(ctx context.Context, h string) {
	hash, _ := hex.DecodeString(h)
	inv, err := p.LN.LookupInvoice(ctx, hash)
	if err != nil {
		return
	}
	set := func(from []string, to string) {
		p.Ledger.Lock()
		defer p.Ledger.Unlock()
		x := p.Ledger.Holds[h]
		if x == nil {
			return
		}
		for _, f := range from {
			if x.State == f {
				if to == "" {
					delete(p.Ledger.Holds, h)
				} else {
					x.State = to
				}
				_ = p.Ledger.save()
				return
			}
		}
	}
	p.Ledger.Lock()
	x := p.Ledger.Holds[h]
	var state string
	var at time.Time
	if x != nil {
		state, at = x.State, x.At
	}
	_, locked := p.Ledger.Lockups[h]
	p.Ledger.Unlock()
	switch {
	case x == nil:
		return
	case state == HoldCreating:
		p.Ledger.Lock()
		sat := x.Sat
		p.Ledger.Unlock()
		p.Ledger.Lock()
		memo := x.Memo
		p.Ledger.Unlock()
		switch {
		case inv != nil && memo != "" && inv.Memo == memo && (inv.ValueMsat+999)/1000 == sat && !inv.Created.Before(at.Add(-2*time.Minute)):
			set([]string{HoldCreating}, "open") // lnd made it; the rest next round
		case inv != nil:
			// an invoice with that hash that is not the one asked for (its
			// amount or age differ: another service's): ours never was made
			set([]string{HoldCreating}, "")
		case p.now().Sub(at) > 2*time.Minute && !p.pendingHold(h):
			set([]string{HoldCreating}, "") // lnd never made it
		}
	case inv == nil, inv.State == InvoiceCanceled:
		set([]string{"open", HoldCanceling}, "canceled")
	case inv.State == InvoiceSettled:
		set([]string{"open", HoldCanceling}, "settled")
	case state == "open" && !locked && inv.State == InvoiceAccepted && p.MaxHoldParts > 0 && len(inv.HTLCExpiries) > p.MaxHoldParts:
		// too many parts hold too many HTLC slots (the bridge's and the
		// LSP's too): cancel it, closed to lockups first, under the lock
		p.mu.Lock()
		defer p.mu.Unlock()
		p.Ledger.Lock()
		x := p.Ledger.Holds[h]
		_, locked := p.Ledger.Lockups[h]
		ok := x != nil && x.State == "open" && !locked
		if ok {
			x.State = HoldCanceling
			_ = p.Ledger.save()
		}
		p.Ledger.Unlock()
		if !ok {
			return
		}
		if p.LN.CancelInvoice(ctx, hash) == nil {
			set([]string{HoldCanceling}, "canceled")
			p.notify("guard_hold_canceled", map[string]any{"hash": h, "htlcs": len(inv.HTLCExpiries)})
		} else {
			set([]string{HoldCanceling}, "open")
		}
	case state == HoldCanceling && !p.cancelPending(h):
		// a cancel allowed whose answer never came: open again only if lnd,
		// asked afresh under the lock, did not cancel it
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.cancelPending(h) {
			return
		}
		inv, err := p.LN.LookupInvoice(ctx, hash)
		if err != nil {
			return
		}
		if inv == nil || inv.State == InvoiceCanceled {
			set([]string{HoldCanceling}, "canceled")
		} else if inv.State == InvoiceSettled {
			set([]string{HoldCanceling}, "settled")
		} else {
			set([]string{HoldCanceling}, "open")
		}
	}
}

func (p *Policy) pendingHold(hash string) bool {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for _, r := range p.pending {
		if r.method == "/invoicesrpc.Invoices/AddHoldInvoice" && r.hash == hash {
			return true
		}
	}
	return false
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

// maxScanBack bounds how far back a recovery looks for a spend.
const maxScanBack = 1500

// claimLeafSpend tells whether sp spends x's lockup through the claim leaf
// (which only our key can sign).
func (p *Policy) claimLeafSpend(x *Payment, sp *Spend) bool {
	hash, _ := hex.DecodeString(x.Hash)
	server, err := p.Keys.Public(x.KeyIndex)
	if err != nil {
		return false
	}
	user, err := p.userKey(x.UserKey)
	if err != nil {
		return false
	}
	out, err := swapscript.Build(swapscript.Params{Kind: swapscript.Submarine, Server: server, User: user, PaymentHash: hash, Timeout: x.Timeout})
	return err == nil && out.SpendsLeaf(sp.Witness, out.ClaimLeaf)
}

// refundExposure is what refunds not final yet stand on: this lockup and
// every other being refunded whose hold invoice is still held (a reorg
// undoing several small refunds costs as much as one big one).
func (p *Policy) refundExposure(ctx context.Context, l *Lockup) int64 {
	p.Ledger.Lock()
	defer p.Ledger.Unlock()
	s := l.Sat
	for h, x := range p.Ledger.Lockups {
		if x == l || x.State != LockRefunding {
			continue
		}
		if hold := p.Ledger.Holds[h]; hold != nil && hold.State != "canceled" {
			s += x.Sat
		}
	}
	return s
}

// settledGrace is how long after the timeout a lockup whose invoice was
// settled but never claimed on chain waits before the guard refunds it.
const settledGrace = 144

// saveOrAlert saves the ledger (caller holds its lock) and alerts once per
// failure kind: a ledger that cannot be written is the operator's emergency.
func (p *Policy) saveOrAlert() {
	if err := p.Ledger.save(); err != nil {
		p.pmu.Lock()
		repeat := p.lastSaveErr == err.Error()
		p.lastSaveErr = err.Error()
		p.pmu.Unlock()
		if !repeat {
			p.notify("guard_ledger_error", map[string]any{"error": err.Error()})
		}
	}
}

// unconfirmed tells whether a transaction is still in no block (the cache
// keeps it to one call per block).
func (p *Policy) unconfirmed(ctx context.Context, txid string) bool {
	h, err := p.Chain.Height(ctx)
	if err != nil {
		return false
	}
	c, err := p.cachedConfs(ctx, txid, h, true)
	return err == nil && c < 1
}
