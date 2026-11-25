package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/tyler-smith/go-bip39"
	"google.golang.org/protobuf/proto"

	"github.com/Kilombino/kiloswap/guard/internal/keys"
	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/swapscript"
)

// ---- fakes ----

type fakeLN struct {
	mu          sync.Mutex
	invoices    map[string]*InvoiceInfo
	payreqs     map[string]*PayReq
	tracks      map[string][2]string // hash -> status, preimage
	settled     [][]byte
	balance     Balance
	addr        string
	walletSends []walletSend
	utxos       []Utxo
	walletErr   error
}

func (f *fakeLN) LookupInvoice(_ context.Context, h []byte) (*InvoiceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invoices[hex.EncodeToString(h)], nil
}
func (f *fakeLN) DecodePayReq(_ context.Context, pr string) (*PayReq, error) {
	if x, ok := f.payreqs[pr]; ok {
		return x, nil
	}
	return nil, fmt.Errorf("invalid invoice")
}
func (f *fakeLN) Balance(context.Context) (Balance, error) { return f.balance, nil }
func (f *fakeLN) SettleInvoice(_ context.Context, pre []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, pre)
	h := sha256.Sum256(pre)
	if inv := f.invoices[hex.EncodeToString(h[:])]; inv != nil {
		inv.State = InvoiceSettled
	}
	return nil
}
func (f *fakeLN) TrackPayment(_ context.Context, h []byte) (int32, string, int64, error) {
	t, ok := f.tracks[hex.EncodeToString(h)]
	if !ok {
		return 0, "", 0, nil
	}
	var s int32
	fmt.Sscan(t[0], &s)
	return s, t[1], 0, nil
}
func (f *fakeLN) CancelInvoice(_ context.Context, h []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inv := f.invoices[hex.EncodeToString(h)]; inv != nil {
		inv.State = InvoiceCanceled
	}
	return nil
}
func (f *fakeLN) NewAddress(context.Context) (string, error) { return f.addr, nil }
func (f *fakeLN) ListUnspent(_ context.Context, minConfs int32) ([]Utxo, error) {
	var out []Utxo
	for _, u := range f.utxos {
		if u.Confs >= int64(minConfs) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeLN) WalletOwns(_ context.Context, txid string, _ int64) (bool, error) {
	for _, w := range f.walletSends {
		if w.txid == txid {
			return true, nil
		}
	}
	return false, nil
}

type walletSend struct {
	txid    string
	address string
	sat     int64
	vout    uint32
	confs   int64
}

func (f *fakeLN) WalletPaid(_ context.Context, pk []byte, sat int64, _ int64) (string, uint32, int64, bool, error) {
	if f.walletErr != nil {
		return "", 0, 0, false, f.walletErr
	}
	for _, w := range f.walletSends {
		a, err := btcutil.DecodeAddress(w.address, &chaincfg.RegressionNetParams)
		if err != nil {
			continue
		}
		wpk, _ := txscript.PayToAddrScript(a)
		if bytes.Equal(wpk, pk) && w.sat == sat {
			return w.txid, w.vout, w.confs, true, nil
		}
	}
	return "", 0, 0, false, nil
}

type fakeChain struct {
	mu        sync.Mutex
	txs       map[string][]Output
	spent     map[string]bool
	spenders  map[string]*Spend
	height    int64
	broadcast []*wire.MsgTx
	scans     [][2]int64
	confs     map[string]int64
	feeRate   float64
}

func (c *fakeChain) Outputs(_ context.Context, txid string) ([]Output, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.txs[txid]
	if !ok {
		return nil, fmt.Errorf("no transaction")
	}
	return o, nil
}
func (c *fakeChain) Output(ctx context.Context, txid string, vout uint32) (*Output, error) {
	o, err := c.Outputs(ctx, txid)
	if err != nil || int(vout) >= len(o) {
		return nil, fmt.Errorf("no output")
	}
	return &o[vout], nil
}
func (c *fakeChain) Unspent(_ context.Context, txid string, vout uint32) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.spent[fmt.Sprintf("%s:%d", txid, vout)], nil
}
func (c *fakeChain) Height(context.Context) (int64, error) { return c.height, nil }
func (c *fakeChain) Spender(_ context.Context, txid string, vout uint32, _ int64) (*Spend, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spenders[fmt.Sprintf("%s:%d", txid, vout)], nil
}
func (c *fakeChain) FeeRate(context.Context) (float64, error) {
	if c.feeRate > 0 {
		return c.feeRate, nil
	}
	return 2, nil
}
func (c *fakeChain) Broadcast(_ context.Context, raw string) (string, error) {
	b, _ := hex.DecodeString(raw)
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.broadcast = append(c.broadcast, &tx)
	// as a node does: the transaction is in the mempool, its input spent
	for _, in := range tx.TxIn {
		op := fmt.Sprintf("%s:%d", in.PreviousOutPoint.Hash, in.PreviousOutPoint.Index)
		c.spent[op] = true
		c.spenders[op] = &Spend{TxID: tx.TxHash().String(), Witness: in.Witness}
	}
	c.mu.Unlock()
	return tx.TxHash().String(), nil
}
func (c *fakeChain) FindOutputs(_ context.Context, pk []byte, sat int64, from, to int64) ([]Found, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scans = append(c.scans, [2]int64{from, to})
	var out []Found
	for id, outs := range c.txs {
		for i, o := range outs {
			h := c.height - o.Confirmations + 1
			if bytes.Equal(o.PkScript, pk) && o.Sat == sat && o.Confirmations > 0 && h >= from && h <= to {
				out = append(out, Found{TxID: id, Vout: uint32(i), Height: h})
			}
		}
	}
	return out, nil
}
func (c *fakeChain) Confirmations(_ context.Context, txid string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.confs[txid]; ok {
		return n, nil
	}
	var n int64
	for _, o := range c.txs[txid] {
		n = max(n, o.Confirmations)
	}
	return n, nil
}

type fakeSwaps struct {
	reverse []ReverseSwap
	swaps   map[string]*Swap
}

func (s *fakeSwaps) ReverseByLockup(_ context.Context, a string) ([]ReverseSwap, error) {
	var out []ReverseSwap
	for _, r := range s.reverse {
		if r.LockupAddress == a {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *fakeSwaps) SwapByInvoice(_ context.Context, inv string) (*Swap, error) {
	return s.swaps[inv], nil
}

type fakeLSP struct{ n int64 }

func (f fakeLSP) Reserved() (int64, error) { return f.n, nil }

// ---- setup ----

type env struct {
	t     *testing.T
	p     *Policy
	ln    *fakeLN
	chain *fakeChain
	swaps *fakeSwaps
	keys  *keys.Keys
	next  uint64
	notes []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ent, _ := bip39.NewEntropy(128)
	words, _ := bip39.NewMnemonic(ent)
	f := filepath.Join(t.TempDir(), "seed")
	os.WriteFile(f, []byte(words), 0o600)
	k, err := keys.FromMnemonicFile(f, "m/0/0")
	if err != nil {
		t.Fatal(err)
	}
	led, _ := OpenLedger("", nil)
	ourAddrKey, _ := btcec.NewPrivateKey()
	a, _ := btcutil.NewAddressTaproot(ourAddrKey.PubKey().SerializeCompressed()[1:], &chaincfg.RegressionNetParams)
	e := &env{t: t, keys: k,
		ln:    &fakeLN{invoices: map[string]*InvoiceInfo{}, payreqs: map[string]*PayReq{}, tracks: map[string][2]string{}, balance: Balance{Confirmed: 300_000_000}, addr: a.EncodeAddress()},
		chain: &fakeChain{txs: map[string][]Output{}, spent: map[string]bool{}, spenders: map[string]*Spend{}, height: 1000, confs: map[string]int64{}},
		swaps: &fakeSwaps{swaps: map[string]*Swap{}},
	}
	e.p = &Policy{
		MaxPerOpSat: 100_000_000, DailySat: 200_000_000, ReserveSat: 50_000_000, MaxPendingReverse: 5,
		FeeRatio: 0.0001, MinFeeSat: 121, MaxFeeRate: 25, RequiredConfs: 3,
		MinLockBlocks: 12, MaxLockBlocks: 4320, HoldExpiryDelta: 18, SafetyBlocks: 6, ClaimMarginSat: 100,
		MinInvoiceSat: 10_000, ClaimMarginBlocks: 40, MinSubTimeoutBlocks: 72,
		ConfTiers:    []ConfTier{{1_000_000, 3}, {10_000_000, 6}, {100_000_000, 12}},
		UrgentBlocks: 24, UrgentFeeFraction: 0.1, MaxHoldParts: 16, ReverseMarginBlocks: 36,
		MaxHoldCltv: 288, LockupFeeFactor: 2, MinReverseSat: 10_000, MaxLockupsPerBlock: 2,
		Net: &chaincfg.RegressionNetParams, Swaps: e.swaps, LN: e.ln, Chain: e.chain, Keys: k,
		LSP: fakeLSP{}, Ledger: led,
	}
	e.p.Notify = func(ev string, _ map[string]any) { e.notes = append(e.notes, ev) }
	return e
}

func (e *env) msg(uri string, m proto.Message) *lnrpc.RPCMessage {
	b, _ := proto.Marshal(m)
	return &lnrpc.RPCMessage{MethodFullUri: uri, Serialized: b}
}

func (e *env) call(uri string, m proto.Message) (uint64, error) {
	e.next++
	return e.next, e.p.Intercept(context.Background(), CaveatCondition, e.next, e.msg(uri, m))
}

func (e *env) answer(id uint64, m proto.Message, isErr bool) {
	var b []byte
	if m != nil {
		b, _ = proto.Marshal(m)
	}
	e.p.Answered(id, &lnrpc.RPCMessage{Serialized: b, IsError: isErr, StreamRpc: true})
}

func mustRefuse(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !Refused(err) {
		t.Fatalf("want a refusal (%s), got %v", want, err)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %q does not say %q", err, want)
	}
}

func mustAllow(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
}

func randKey() *btcec.PrivateKey { k, _ := btcec.NewPrivateKey(); return k }

func preimage(b byte) ([]byte, []byte) {
	p := bytes.Repeat([]byte{b}, 32)
	h := sha256.Sum256(p)
	return p, h[:]
}

func addrOf(t *testing.T, pk []byte) string {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(pk, &chaincfg.RegressionNetParams)
	if err != nil || len(addrs) != 1 {
		t.Fatal(err)
	}
	return addrs[0].EncodeAddress()
}

// submarine sets up a submarine swap whose lockup pays lockSat, and its
// invoice for invSat; returns the invoice string.
func (e *env) submarine(idx uint32, invSat, lockSat int64, b byte) (string, *Swap, []byte) {
	pre, hash := preimage(b)
	user := randKey()
	server, _ := e.keys.Public(idx)
	out, _ := swapscript.Build(swapscript.Params{Kind: swapscript.Submarine, Server: server, User: user.PubKey(), PaymentHash: hash, Timeout: 1100})
	txid := strings.Repeat(fmt.Sprintf("%02x", b), 32)
	e.chain.txs[txid] = []Output{{PkScript: out.PkScript, Sat: lockSat, Confirmations: 3}}
	inv := fmt.Sprintf("lnbcrt%dinv%d", invSat, b)
	e.ln.payreqs[inv] = &PayReq{Hash: hash, AmountMsat: invSat * 1000}
	s := &Swap{ID: fmt.Sprintf("sub%d", b), PreimageHash: hex.EncodeToString(hash), RefundKey: hex.EncodeToString(user.PubKey().SerializeCompressed()),
		KeyIndex: int64(idx), Timeout: 1100, LockupTxID: txid, LockupVout: 0, Version: 1}
	e.swaps.swaps[inv] = s
	return inv, s, pre
}

func payReq(inv string) *lnrpc.SendPaymentRequest {
	return &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121, CltvLimit: 50, TimeoutSeconds: 60}
}

const payURI = "/routerrpc.Router/SendPaymentV2"

// ---- payments (review issues 1 and 2) ----

func TestPaymentAmountFromTheInvoiceNotTheDatabase(t *testing.T) {
	e := newEnv(t)
	// the attacker's 5 BTC invoice; the database can say anything
	inv, _, _ := e.submarine(1, 500_000_000, 1000, 1)
	_, err := e.call(payURI, payReq(inv))
	mustRefuse(t, err, "limit per operation")
	// 0.5 BTC invoice against a 1000 sat lockup
	inv, _, _ = e.submarine(2, 50_000_000, 1000, 2)
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "less than")
}

func TestPaymentLockupMustBeTheRebuiltOutput(t *testing.T) {
	e := newEnv(t)
	inv, s, _ := e.submarine(3, 100_000, 200_000, 3)
	// the lockup pays an address the attacker controls instead
	e.chain.txs[s.LockupTxID][0].PkScript, _ = txscript.PayToTaprootScript(randKey().PubKey())
	_, err := e.call(payURI, payReq(inv))
	mustRefuse(t, err, "rebuilds")
	// hash of the swap row differs from the invoice's
	inv, s, _ = e.submarine(4, 100_000, 200_000, 4)
	s.PreimageHash = strings.Repeat("00", 32)
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "hash")
}

func TestPaymentChecks(t *testing.T) {
	e := newEnv(t)
	inv, s, _ := e.submarine(5, 100_000, 200_000, 5)
	r := payReq(inv)
	r.CltvLimit = 0
	_, err := e.call(payURI, r)
	mustRefuse(t, err, "cltv_limit")
	r = payReq(inv)
	r.CltvLimit = 95 // timeout 1100, height 1000, margin 40: lowered to 60
	id, err := e.call(payURI, r)
	mustAllow(t, err)
	var q lnrpc.SendPaymentRequest
	if proto.Unmarshal(e.p.Replacement(id), &q) != nil || q.CltvLimit != 60 || q.MaxParts != MaxPayParts {
		t.Fatalf("cltv_limit 95 rewritten to %d (parts %d)", q.CltvLimit, q.MaxParts)
	}
	e.answer(id, &lnrpc.Payment{Status: paymentFailed}, false)
	r = payReq(inv)
	r.OutgoingChanIds = []uint64{1}
	_, err = e.call(payURI, r)
	mustRefuse(t, err, "outgoing")
	r = payReq(inv)
	r.FeeLimitSat = 5000 // over max(MinFeeSat, FeeRatio): lowered, never refused
	id, err = e.call(payURI, r)
	mustAllow(t, err)
	q = lnrpc.SendPaymentRequest{}
	if proto.Unmarshal(e.p.Replacement(id), &q) != nil || q.FeeLimitSat != 0 || q.FeeLimitMsat <= 0 || q.FeeLimitMsat >= 5_000_000 {
		t.Fatalf("fee limit 5000 sat rewritten to %d msat (sat %d)", q.FeeLimitMsat, q.FeeLimitSat)
	}
	e.answer(id, &lnrpc.Payment{Status: paymentFailed}, false)
	e.chain.txs[s.LockupTxID][0].Confirmations = 2
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "confirmations")
	e.chain.txs[s.LockupTxID][0].Confirmations = 3
	e.chain.spent[s.LockupTxID+":0"] = true
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "spent")
	e.chain.spent[s.LockupTxID+":0"] = false
	_, err = e.call(payURI, payReq(inv))
	mustAllow(t, err)
}

func TestLockupBacksOnePayment(t *testing.T) {
	e := newEnv(t)
	inv, s, _ := e.submarine(6, 100_000, 200_000, 6)
	_, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	// another swap row pointing at the same lockup
	inv2, s2, _ := e.submarine(7, 100_000, 200_000, 7)
	s2.LockupTxID = s.LockupTxID
	e.chain.txs[s.LockupTxID] = append(e.chain.txs[s.LockupTxID][:1], e.chain.txs[s2.LockupTxID]...)
	_ = inv2
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "already")
}

func TestCapNotGivenBackOnStreamErrors(t *testing.T) {
	e := newEnv(t)
	e.p.DailySat = 150_000
	inv, _, _ := e.submarine(8, 100_000, 200_000, 8)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	// the backend cancels the stream / lnd answers AlreadyExists
	e.answer(id, nil, true)
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "already")
	// another payment would pass the daily limit: the first still counts
	inv2, _, _ := e.submarine(9, 100_000, 200_000, 9)
	_, err = e.call(payURI, payReq(inv2))
	mustRefuse(t, err, "daily limit")
	// a FAILED on some other request id changes nothing
	e.answer(9999, &lnrpc.Payment{Status: paymentFailed}, false)
	_, err = e.call(payURI, payReq(inv2))
	mustRefuse(t, err, "daily limit")
	// lnd itself says the first one failed: given back
	_, hash := preimage(8)
	e.ln.tracks[hex.EncodeToString(hash)] = [2]string{"3", ""}
	e.p.reconcilePayments(context.Background())
	_, err = e.call(payURI, payReq(inv2))
	mustAllow(t, err)
}

func TestFailedOnOwnStreamReleases(t *testing.T) {
	e := newEnv(t)
	e.p.DailySat = 150_000
	inv, _, _ := e.submarine(10, 100_000, 200_000, 10)
	id, _ := e.call(payURI, payReq(inv))
	e.answer(id, &lnrpc.Payment{Status: paymentInFlight}, false)
	e.answer(id, &lnrpc.Payment{Status: paymentFailed}, false)
	// retry allowed (same lockup, the payment never went through)
	_, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
}

// ---- claims ----

func verify(t *testing.T, tx *wire.MsgTx, prevPk []byte, prevSat int64) {
	t.Helper()
	fetch := txscript.NewCannedPrevOutputFetcher(prevPk, prevSat)
	vm, err := txscript.NewEngine(prevPk, tx, 0, txscript.StandardVerifyFlags, nil, txscript.NewTxSigHashes(tx, fetch), prevSat, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("the guard's transaction does not verify: %v", err)
	}
}

func TestPaidThenClaimed(t *testing.T) {
	e := newEnv(t)
	inv, s, pre := e.submarine(11, 100_000, 200_000, 11)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: hex.EncodeToString(pre)}, false)
	txid, fee, err := e.p.Claim(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.chain.broadcast) != 1 || txid == "" || fee <= 0 {
		t.Fatal("no claim broadcast")
	}
	tx := e.chain.broadcast[0]
	o := e.chain.txs[s.LockupTxID][0]
	verify(t, tx, o.PkScript, o.Sat)
	if tx.TxOut[0].Value != o.Sat-fee || addrOf(t, tx.TxOut[0].PkScript) != e.ln.addr {
		t.Fatal("the claim does not go to lnd's address")
	}
	// asking again returns the same claim
	txid2, _, _ := e.p.Claim(context.Background(), s.ID)
	if txid2 != txid || len(e.chain.broadcast) != 1 {
		t.Fatal("claimed twice")
	}
}

func TestWrongPreimageIsNotTaken(t *testing.T) {
	e := newEnv(t)
	inv, s, _ := e.submarine(12, 100_000, 200_000, 12)
	id, _ := e.call(payURI, payReq(inv))
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: strings.Repeat("ab", 32)}, false)
	if _, _, err := e.p.Claim(context.Background(), s.ID); err == nil {
		t.Fatal("claimed with a preimage that does not match")
	}
}

// ---- reverse swaps (review issues 3, 4, 6) ----

const (
	holdURI   = "/invoicesrpc.Invoices/AddHoldInvoice"
	sendURI   = "/lnrpc.Lightning/SendCoins"
	cancelURI = "/invoicesrpc.Invoices/CancelInvoice"
	lookupURI = "/lnrpc.Lightning/LookupInvoice"
)

// reverse creates a hold invoice through the guard, paid and held by lnd,
// and the backend's row for its lockup.
func (e *env) reverse(idx uint32, sat int64, b byte) (ReverseSwap, []byte, *btcec.PrivateKey) {
	pre, hash := preimage(b)
	id, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: sat + 2000})
	if err != nil {
		e.t.Fatal(err)
	}
	e.answer(id, &lnrpc.AddHoldInvoiceResp{PaymentRequest: "lnbcrt"}, false)
	e.p.Ledger.Lock()
	memo := e.p.Ledger.Holds[hex.EncodeToString(hash)].Memo // lnd keeps the guard's memo
	e.p.Ledger.Unlock()
	e.ln.invoices[hex.EncodeToString(hash)] = &InvoiceInfo{State: InvoiceAccepted, Memo: memo, PaidMsat: (sat + 2000) * 1000, HTLCExpiries: []int32{1200}}
	user := randKey()
	server, _ := e.keys.Public(idx)
	out, _ := swapscript.Build(swapscript.Params{Kind: swapscript.Reverse, Server: server, User: user.PubKey(), PaymentHash: hash, Timeout: 1100})
	r := ReverseSwap{ID: fmt.Sprintf("rev%d", b), LockupAddress: addrOf(e.t, out.PkScript), OnchainAmount: sat,
		PreimageHash: hex.EncodeToString(hash), ClaimKey: hex.EncodeToString(user.PubKey().SerializeCompressed()),
		KeyIndex: int64(idx), Timeout: 1100, Version: 1}
	e.swaps.reverse = append(e.swaps.reverse, r)
	return r, pre, user
}

func sendReq(r ReverseSwap) *lnrpc.SendCoinsRequest {
	return &lnrpc.SendCoinsRequest{Addr: r.LockupAddress, Amount: r.OnchainAmount, SatPerByte: 2}
}

func TestLockupNeedsAGuardHoldInvoice(t *testing.T) {
	e := newEnv(t)
	// a hold invoice created elsewhere (the bridge's, the LSP's): paid, held
	pre, hash := preimage(20)
	_ = pre
	e.ln.invoices[hex.EncodeToString(hash)] = &InvoiceInfo{State: InvoiceAccepted, PaidMsat: 10_000_000_000, HTLCExpiries: []int32{1200}}
	user := randKey()
	server, _ := e.keys.Public(20)
	out, _ := swapscript.Build(swapscript.Params{Kind: swapscript.Reverse, Server: server, User: user.PubKey(), PaymentHash: hash, Timeout: 1100})
	r := ReverseSwap{ID: "x", LockupAddress: addrOf(t, out.PkScript), OnchainAmount: 1_000_000, PreimageHash: hex.EncodeToString(hash),
		ClaimKey: hex.EncodeToString(user.PubKey().SerializeCompressed()), KeyIndex: 20, Timeout: 1100, Version: 1}
	e.swaps.reverse = append(e.swaps.reverse, r)
	_, err := e.call(sendURI, sendReq(r))
	mustRefuse(t, err, "not created through the guard")
}

func TestLockupChecks(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(21, 1_000_000, 21)
	// send_all, unconfirmed, target_conf
	_, err := e.call(sendURI, &lnrpc.SendCoinsRequest{Addr: r.LockupAddress, SendAll: true, SatPerByte: 2})
	mustRefuse(t, err, "send_all")
	_, err = e.call(sendURI, &lnrpc.SendCoinsRequest{Addr: r.LockupAddress, Amount: r.OnchainAmount, TargetConf: 2})
	mustRefuse(t, err, "fee rate")
	// a row pointing to an address that is not the rebuilt output
	bad := r
	pk, _ := txscript.PayToTaprootScript(randKey().PubKey())
	bad.LockupAddress = addrOf(t, pk)
	e.swaps.reverse = append(e.swaps.reverse, bad)
	_, err = e.call(sendURI, sendReq(bad))
	mustRefuse(t, err, "rebuilds")
	// timeout too far
	r2, _, _ := e.reverse(22, 1_000_000, 22)
	e.swaps.reverse[len(e.swaps.reverse)-1].Timeout = 9000
	_, err = e.call(sendURI, sendReq(r2))
	mustRefuse(t, err, "")
	// HTLC expiring too close to the timeout
	r3, _, _ := e.reverse(23, 1_000_000, 23)
	_, h3 := preimage(23)
	e.ln.invoices[hex.EncodeToString(h3)].HTLCExpiries = []int32{1110}
	_, err = e.call(sendURI, sendReq(r3))
	mustRefuse(t, err, "too close")
	// the good one
	_, err = e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	// the same hash twice
	_, err = e.call(sendURI, sendReq(r))
	mustRefuse(t, err, "already has a lockup")
}

func TestConcurrentSendsKeepTheReserve(t *testing.T) {
	e := newEnv(t)
	e.ln.balance = Balance{Confirmed: 52_900_000}
	r1, _, _ := e.reverse(24, 1_500_000, 24)
	_, err := e.call(sendURI, sendReq(r1))
	mustAllow(t, err)
	// lnd has not answered the send yet, so its coins are still in the
	// balance: they must not be promised again
	_, hash := preimage(25)
	_, err = e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 1_501_000})
	mustRefuse(t, err, "would not fit")
}

func TestCancelOnlyOwnAndNotWhileLockedUp(t *testing.T) {
	e := newEnv(t)
	_, other := preimage(30)
	_, err := e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: other})
	mustRefuse(t, err, "not a Kiloswap")
	r, _, _ := e.reverse(31, 1_000_000, 31)
	_, hash := preimage(31)
	id, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	e.answer(id, &lnrpc.SendCoinsResponse{Txid: strings.Repeat("31", 32)}, false)
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: hash})
	mustRefuse(t, err, "stays until")
	// a hold invoice the backend tried to add with an existing hash is never its own
	e.ln.invoices[hex.EncodeToString(other)] = &InvoiceInfo{State: InvoiceAccepted}
	_, err = e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: other, Expiry: 3600, Value: 20000})
	mustRefuse(t, err, "already exists")
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: other})
	mustRefuse(t, err, "not a Kiloswap")
}

func TestHoldRecordedOnlyWhenLndCreatedIt(t *testing.T) {
	e := newEnv(t)
	_, hash := preimage(40)
	id, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 20000})
	mustAllow(t, err)
	e.answer(id, nil, true) // lnd refused it
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: hash})
	mustRefuse(t, err, "not a Kiloswap")
	_, err = e.call(lookupURI, &lnrpc.PaymentHash{RHash: hash})
	mustRefuse(t, err, "not an invoice")
}

func TestPendingCountIsTheGuards(t *testing.T) {
	e := newEnv(t)
	e.p.MaxPendingReverse = 2
	e.reverse(41, 100_000, 41)
	e.reverse(42, 100_000, 42)
	_, hash := preimage(43)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 20000})
	mustRefuse(t, err, "pending")
}

// ---- review issue 5 ----

func TestLookupsOnlyOwnInvoices(t *testing.T) {
	e := newEnv(t)
	_, other := preimage(50)
	_, err := e.call(lookupURI, &lnrpc.PaymentHash{RHash: other})
	mustRefuse(t, err, "not an invoice")
	_, err = e.call("/invoicesrpc.Invoices/SubscribeSingleInvoice", &lnrpc.SubscribeSingleInvoiceRequest{RHash: other})
	mustRefuse(t, err, "not an invoice")
	// an invoice the backend added through AddInvoice is its own
	id, err := e.call("/lnrpc.Lightning/AddInvoice", &lnrpc.Invoice{Value: 1})
	mustAllow(t, err)
	e.answer(id, &lnrpc.AddInvoiceResponse{RHash: other}, false)
	_, err = e.call(lookupURI, &lnrpc.PaymentHash{RHash: other})
	mustAllow(t, err)
	_, err = e.call("/routerrpc.Router/ResetMissionControl", &lnrpc.WalletBalanceRequest{})
	mustRefuse(t, err, "not allowed")
}

// ---- the guard's own work on reverse lockups ----

func (e *env) sent(r ReverseSwap, b byte) (string, []byte) {
	id, err := e.call(sendURI, sendReq(r))
	mustAllow(e.t, err)
	txid := strings.Repeat(fmt.Sprintf("%02x", b), 32)
	e.answer(id, &lnrpc.SendCoinsResponse{Txid: txid}, false)
	pk, _ := e.p.addrScript(r.LockupAddress)
	e.chain.txs[txid] = []Output{{PkScript: []byte{0x51}, Sat: 1}, {PkScript: pk, Sat: r.OnchainAmount, Confirmations: 1}}
	return txid, pk
}

func TestUserClaimSettles(t *testing.T) {
	e := newEnv(t)
	r, pre, user := e.reverse(60, 1_000_000, 60)
	txid, _ := e.sent(r, 60)
	_ = user
	e.p.Tick(context.Background())
	_, hash := preimage(60)
	server, _ := e.keys.Public(60)
	out, _ := swapscript.Build(swapscript.Params{Kind: swapscript.Reverse, Server: server, User: user.PubKey(), PaymentHash: hash, Timeout: 1100})
	cb, _ := out.ControlBlock(out.ClaimLeaf)
	e.chain.spent[txid+":1"] = true
	e.chain.spenders[txid+":1"] = &Spend{TxID: "claim", Witness: [][]byte{make([]byte, 64), pre, out.ClaimLeaf.Script, cb}}
	e.p.Tick(context.Background())
	if len(e.ln.settled) != 1 || !bytes.Equal(e.ln.settled[0], pre) {
		t.Fatal("the hold invoice was not settled with the claim's preimage")
	}
	if e.p.Ledger.Lockups[hex.EncodeToString(hash)].State != LockSettled {
		t.Fatal("lockup not settled")
	}
}

func TestRefundAfterTimeout(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(61, 1_000_000, 61)
	txid, pk := e.sent(r, 61)
	if _, _, err := e.p.Refund(context.Background(), r.ID); err == nil {
		t.Fatal("refunded before the timeout")
	}
	e.chain.height = 1100
	e.p.Tick(context.Background())
	if len(e.chain.broadcast) != 1 {
		t.Fatal("no refund")
	}
	tx := e.chain.broadcast[0]
	if tx.LockTime != 1100 {
		t.Fatal("refund without the lock time")
	}
	verify(t, tx, pk, r.OnchainAmount)
	_, hash := preimage(61)
	_, err := e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: hash})
	mustRefuse(t, err, "stays until")
	e.chain.spent[txid+":1"] = true
	e.chain.spenders[txid+":1"] = &Spend{TxID: tx.TxHash().String(), Witness: tx.TxIn[0].Witness, Confirmations: 1}
	e.p.Tick(context.Background())
	if e.ln.invoices[hex.EncodeToString(hash)].State == InvoiceCanceled {
		t.Fatal("cancelled at 1 confirmation: a reorg could undo the refund")
	}
	e.chain.spenders[txid+":1"].Confirmations = 3
	e.p.Tick(context.Background())
	if e.ln.invoices[hex.EncodeToString(hash)].State != InvoiceCanceled {
		t.Fatal("the guard did not cancel the hold invoice after its refund confirmed")
	}
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: hash})
	mustAllow(t, err)
}

func TestKeysNeverLeaveTheGuard(t *testing.T) {
	e := newEnv(t)
	x, err := e.keys.AccountXpub()
	if err != nil || !strings.HasPrefix(x, "xpub") {
		t.Fatalf("xpub: %v %s", err, x)
	}
	// the backend derives our public keys from it, the same ones
	ek, _ := btcutilHD(x)
	pub, _ := e.keys.Public(5)
	if !bytes.Equal(ek(5), pub.SerializeCompressed()) {
		t.Fatal("the xpub does not give our keys")
	}
	_ = time.Now
}

func btcutilHD(xpub string) (func(uint32) []byte, error) {
	k, err := hdkeychain.NewKeyFromString(xpub)
	if err != nil {
		return nil, err
	}
	return func(i uint32) []byte {
		c, _ := k.Derive(i)
		p, _ := c.ECPubKey()
		return p.SerializeCompressed()
	}, nil
}

func feeOf(tx *wire.MsgTx, prevSat int64) int64 { return prevSat - tx.TxOut[0].Value }

// A claim that stays unconfirmed is replaced with a higher fee to the same
// address after bumpEvery blocks; one that fell out of the mempool goes
// again; the one that confirms (even an older one) closes it.
func TestClaimBumped(t *testing.T) {
	e := newEnv(t)
	inv, s, pre := e.submarine(12, 100_000, 200_000, 12)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: hex.EncodeToString(pre)}, false)
	ctx := context.Background()
	if _, _, err := e.p.Claim(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	first := e.chain.broadcast[0]
	o := e.chain.txs[s.LockupTxID][0]
	var x *Payment
	for _, y := range e.p.Ledger.Payments {
		x = y
	}
	op := fmt.Sprintf("%s:%d", x.LockupTx, x.Vout)
	dest := e.ln.addr
	e.ln.addr = "" // a replacement must not ask lnd for a new address
	// in the mempool, not long enough: nothing
	e.chain.spent[op] = true
	e.chain.spenders[op] = &Spend{TxID: first.TxHash().String(), Witness: first.TxIn[0].Witness}
	e.chain.height += bumpEvery - 1
	e.p.Tick(ctx)
	if len(e.chain.broadcast) != 1 {
		t.Fatal("replaced too early")
	}
	e.chain.height++
	e.p.Tick(ctx)
	if len(e.chain.broadcast) != 2 {
		t.Fatal("not replaced after bumpEvery blocks")
	}
	second := e.chain.broadcast[1]
	verify(t, second, o.PkScript, o.Sat)
	if feeOf(second, o.Sat) <= feeOf(first, o.Sat) || addrOf(t, second.TxOut[0].PkScript) != dest {
		t.Fatalf("replacement fee %d (was %d) to %s", feeOf(second, o.Sat), feeOf(first, o.Sat), addrOf(t, second.TxOut[0].PkScript))
	}
	if second.TxIn[0].Sequence >= wire.MaxTxInSequenceNum-1 {
		t.Fatal("the replacement does not signal RBF")
	}
	// dropped from the mempool: broadcast again at once
	delete(e.chain.spent, op)
	delete(e.chain.spenders, op)
	e.p.Tick(ctx)
	if len(e.chain.broadcast) != 3 {
		t.Fatal("a dropped claim was not broadcast again")
	}
	// the first one confirms after all: done, no more replacements
	e.chain.spent[op] = true
	e.chain.spenders[op] = &Spend{TxID: first.TxHash().String(), Witness: first.TxIn[0].Witness, Confirmations: 3}
	e.chain.height += 10
	e.p.Tick(ctx)
	e.p.Tick(ctx)
	if len(e.chain.broadcast) != 3 || !x.Bump.Done {
		t.Fatal("kept replacing a confirmed claim")
	}
}

// The user refunding the lockup (spent by someone else) stops the guard and
// alerts.
func TestClaimLostAlerts(t *testing.T) {
	e := newEnv(t)
	inv, s, pre := e.submarine(13, 100_000, 200_000, 13)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: hex.EncodeToString(pre)}, false)
	ctx := context.Background()
	if _, _, err := e.p.Claim(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	var x *Payment
	for _, y := range e.p.Ledger.Payments {
		x = y
	}
	op := fmt.Sprintf("%s:%d", x.LockupTx, x.Vout)
	e.chain.spent[op] = true
	e.chain.spenders[op] = &Spend{TxID: strings.Repeat("ab", 32), Confirmations: 1}
	e.p.Tick(ctx)
	e.p.Tick(ctx)
	if !x.Bump.Done || len(e.chain.broadcast) != 1 {
		t.Fatal("kept claiming a lockup someone else spent")
	}
}

// Our refund stuck in the mempool is replaced (same lock time, same
// address); the replaced one confirming still counts as our refund.
func TestRefundBumped(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(62, 1_000_000, 62)
	txid, pk := e.sent(r, 62)
	ctx := context.Background()
	e.chain.height = 1100
	e.p.Tick(ctx)
	if len(e.chain.broadcast) != 1 {
		t.Fatal("no refund")
	}
	first := e.chain.broadcast[0]
	op := txid + ":1"
	e.ln.addr = ""
	e.chain.spent[op] = true
	e.chain.spenders[op] = &Spend{TxID: first.TxHash().String(), Witness: first.TxIn[0].Witness}
	e.chain.height += bumpEvery
	e.p.Tick(ctx)
	if len(e.chain.broadcast) != 2 {
		t.Fatal("refund not replaced")
	}
	second := e.chain.broadcast[1]
	verify(t, second, pk, r.OnchainAmount)
	if second.LockTime != first.LockTime || feeOf(second, r.OnchainAmount) <= feeOf(first, r.OnchainAmount) ||
		!bytes.Equal(second.TxOut[0].PkScript, first.TxOut[0].PkScript) {
		t.Fatal("the replacement changed the lock time or address, or did not raise the fee")
	}
	e.chain.spenders[op] = &Spend{TxID: first.TxHash().String(), Witness: first.TxIn[0].Witness, Confirmations: 3}
	e.p.Tick(ctx)
	_, hash := preimage(62)
	if e.ln.invoices[hex.EncodeToString(hash)].State != InvoiceCanceled {
		t.Fatal("the replaced refund confirming was not taken as ours")
	}
}

func TestReplacementRate(t *testing.T) {
	b := Bump{Rate: 2, Height: 100}
	if _, ok := replacementRate(b, 101, true, 25); ok {
		t.Fatal("bumped too early")
	}
	if r, ok := replacementRate(b, 103, true, 25); !ok || r != 4 {
		t.Fatalf("got %v %v", r, ok)
	}
	if r, ok := replacementRate(b, 101, false, 25); !ok || r != 2 {
		t.Fatal("a dropped spend goes again at its rate")
	}
	if _, ok := replacementRate(Bump{Rate: 25, Height: 100}, 200, true, 25); ok {
		t.Fatal("bumped past the cap")
	}
}

// lnd's answer to an allowed lockup never reaches the guard (a stalled
// stream): the guard finds the lockup on chain by its output and amount and
// still settles from the user's claim.
func TestApprovedLockupFoundWithoutAnswer(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	r, pre, user := e.reverse(63, 1_000_000, 63)
	_, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err) // no answer from lnd follows
	txid := strings.Repeat("63", 32)
	pk, _ := e.p.addrScript(r.LockupAddress)
	e.chain.txs[txid] = []Output{{PkScript: []byte{0x51}, Sat: 1}, {PkScript: pk, Sat: r.OnchainAmount, Confirmations: 1}}
	// a decoy paying the same address and amount, not from lnd's wallet
	e.chain.txs[strings.Repeat("de", 32)] = []Output{{PkScript: pk, Sat: r.OnchainAmount, Confirmations: 5}}
	e.ln.walletSends = append(e.ln.walletSends, walletSend{txid: txid})
	_, hash := preimage(63)
	l := e.p.Ledger.Lockups[hex.EncodeToString(hash)]
	e.p.Tick(context.Background())
	if l.State != LockApproved {
		t.Fatal("looked for the lockup before giving lnd time to answer")
	}
	at = at.Add(3 * time.Minute)
	e.p.Tick(context.Background())
	if l.State != LockSent || l.TxID != txid || l.Vout != 1 {
		t.Fatalf("lockup not found on chain: %+v", l)
	}
	server, _ := e.keys.Public(63)
	out, _ := swapscript.Build(swapscript.Params{Kind: swapscript.Reverse, Server: server, User: user.PubKey(), PaymentHash: hash, Timeout: 1100})
	cb, _ := out.ControlBlock(out.ClaimLeaf)
	e.chain.spent[txid+":1"] = true
	e.chain.spenders[txid+":1"] = &Spend{TxID: "claim", Witness: [][]byte{make([]byte, 64), pre, out.ClaimLeaf.Script, cb}}
	e.p.Tick(context.Background())
	if len(e.ln.settled) != 1 || !bytes.Equal(e.ln.settled[0], pre) {
		t.Fatal("not settled from the claim of a lockup found without lnd's answer")
	}
}

// A judgment refused for coming late leaves nothing behind.
func TestForgetUndoesALateJudgment(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(64, 1_000_000, 64)
	id, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	_, hash := preimage(64)
	if e.p.Ledger.Lockups[hex.EncodeToString(hash)] == nil {
		t.Fatal("no lockup recorded")
	}
	e.p.Forget(id)
	if e.p.Ledger.Lockups[hex.EncodeToString(hash)] != nil || e.p.Ledger.since(time.Time{}) != 0 {
		t.Fatal("a refused-late lockup stays recorded")
	}
	inv, s, _ := e.submarine(65, 100_000, 200_000, 65)
	id, err = e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.p.Forget(id)
	if e.p.Ledger.Payments[s.PreimageHash] != nil || len(e.p.Ledger.Outpoints) != 0 {
		t.Fatal("a refused-late payment stays recorded")
	}
}

// A payment whose stream still looks open is asked about anyway after two
// minutes: a stalled stream must not hide a success that has to be claimed.
func TestInflightTrackedEvenWithStreamOpen(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	inv, s, pre := e.submarine(66, 100_000, 200_000, 66)
	_, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.ln.tracks[s.PreimageHash] = [2]string{"2", hex.EncodeToString(pre)}
	at = at.Add(3 * time.Minute)
	e.p.Tick(context.Background())
	if len(e.chain.broadcast) != 1 {
		t.Fatalf("a payment that succeeded behind an open stream was not claimed: %v %+v", e.notes, e.p.Ledger.Payments[s.PreimageHash])
	}
}

// ---- the second review: margins, minimums, depth, griefing ----

func TestSubmarineMinimumsAndMargins(t *testing.T) {
	e := newEnv(t)
	inv, _, _ := e.submarine(70, 9_999, 200_000, 70)
	_, err := e.call(payURI, payReq(inv))
	mustRefuse(t, err, "minimum")
	// the lockup must cover routing and our claim at the highest fee rate
	inv, _, _ = e.submarine(71, 100_000, 100_000+121+100, 71)
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "plus our claim cost")
	// cltv_limit must leave ClaimMarginBlocks before the timeout
	inv, _, _ = e.submarine(72, 100_000, 200_000, 72)
	r := payReq(inv)
	r.CltvLimit = 61 // timeout 1100, height 1000: 100 - 40 = 60 at most
	id, err := e.call(payURI, r)
	mustAllow(t, err) // lowered to 60 by the rewrite
	var q lnrpc.SendPaymentRequest
	if proto.Unmarshal(e.p.Replacement(id), &q) != nil || q.CltvLimit != 60 {
		t.Fatalf("cltv_limit 61 rewritten to %d", q.CltvLimit)
	}
	e.answer(id, &lnrpc.Payment{Status: paymentFailed}, false)
	r.CltvLimit = 60
	r.TimeoutSeconds = 3600
	_, err = e.call(payURI, r)
	mustRefuse(t, err, "timeout_seconds")
	r.TimeoutSeconds = 60
	_, err = e.call(payURI, r)
	mustAllow(t, err)
	// a timeout too close: refused whatever the cltv_limit
	inv, _, _ = e.submarine(73, 100_000, 200_000, 73)
	e.chain.height = 1040 // 60 blocks left, 72 needed
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "at least 72")
}

func TestConfirmationsScaleWithAmount(t *testing.T) {
	e := newEnv(t)
	if e.p.confsFor(500_000) != 3 || e.p.confsFor(5_000_000) != 6 || e.p.confsFor(99_000_000) != 12 || e.p.confsFor(200_000_000) != 12 {
		t.Fatal("tiers")
	}
	inv, s, _ := e.submarine(74, 5_000_000, 5_100_000, 74)
	_, err := e.call(payURI, payReq(inv))
	mustRefuse(t, err, "6 needed")
	e.chain.txs[s.LockupTxID][0].Confirmations = 6
	_, err = e.call(payURI, payReq(inv))
	mustAllow(t, err)
}

func TestLockupFeeCappedByEstimate(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(75, 1_000_000, 75)
	req := sendReq(r)
	req.SatPerByte = 5 // estimate 2 sat/vB: at most 4
	_, err := e.call(sendURI, req)
	mustRefuse(t, err, "estimate")
	req.SatPerByte = 4
	_, err = e.call(sendURI, req)
	mustAllow(t, err)
}

func TestHoldInvoiceGriefingLimits(t *testing.T) {
	e := newEnv(t)
	_, hash := preimage(76)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 50_000, CltvExpiry: 1000})
	mustRefuse(t, err, "cltv_expiry")
	// many HTLC parts: no lockup
	r, _, _ := e.reverse(77, 1_000_000, 77)
	_, h := preimage(77)
	inv := e.ln.invoices[hex.EncodeToString(h)]
	inv.HTLCExpiries = make([]int32, 17)
	for i := range inv.HTLCExpiries {
		inv.HTLCExpiries[i] = 1200
	}
	_, err = e.call(sendURI, sendReq(r))
	mustRefuse(t, err, "HTLCs")
	// an HTLC expiring within timeout + delta + margin: no lockup
	inv.HTLCExpiries = []int32{1100 + 18 + 36}
	_, err = e.call(sendURI, sendReq(r))
	mustRefuse(t, err, "too close")
}

// Hold invoices allowed but not yet created by lnd count against the limit.
func TestHoldCountReservedAtOnce(t *testing.T) {
	e := newEnv(t)
	for i := byte(0); i < 5; i++ {
		_, hash := preimage(80 + i)
		_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 50_000})
		mustAllow(t, err) // no answers from lnd yet
	}
	_, hash := preimage(90)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 50_000})
	mustRefuse(t, err, "pending")
}

// Close to the timeout a claim pays past MAX_FEE_RATE (up to a fraction of
// the lockup) and is replaced every block.
func TestUrgentClaimFee(t *testing.T) {
	e := newEnv(t)
	inv, s, pre := e.submarine(78, 100_000, 200_000, 78)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: hex.EncodeToString(pre)}, false)
	e.chain.height = 1090 // 10 blocks before the timeout
	if _, _, err := e.p.Claim(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	o := e.chain.txs[s.LockupTxID][0]
	first := e.chain.broadcast[0]
	if feeOf(first, o.Sat) > o.Sat/10 {
		t.Fatal("an urgent claim paid over the fraction")
	}
	e.chain.height++
	e.p.Tick(context.Background())
	if len(e.chain.broadcast) != 2 {
		t.Fatal("an urgent claim was not replaced the next block")
	}
	second := e.chain.broadcast[1]
	vsize := int64((second.SerializeSizeStripped()*3 + second.SerializeSize() + 3) / 4)
	if rate := float64(feeOf(second, o.Sat)) / float64(vsize); rate <= float64(e.p.MaxFeeRate)*1.5 {
		t.Fatalf("the urgent replacement stayed under MAX_FEE_RATE: %.1f", rate)
	}
}

// ClaimVbytes really bounds the guard's claim.
func TestClaimVbytesBound(t *testing.T) {
	e := newEnv(t)
	inv, s, pre := e.submarine(79, 100_000, 200_000, 79)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: hex.EncodeToString(pre)}, false)
	if _, _, err := e.p.Claim(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	tx := e.chain.broadcast[0]
	if v := (tx.SerializeSizeStripped()*3 + tx.SerializeSize() + 3) / 4; v > ClaimVbytes {
		t.Fatalf("claim is %d vB, over ClaimVbytes %d", v, ClaimVbytes)
	}
}

// ---- the third review ----

// A cancel allowed first closes the invoice to lockups at once (same lock),
// and a cancel lnd refuses opens it again.
func TestCancelThenLockupRace(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(100, 1_000_000, 100)
	_, hash := preimage(100)
	cid, err := e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: hash})
	mustAllow(t, err)
	_, err = e.call(sendURI, sendReq(r))
	mustRefuse(t, err, "no longer open")
	e.p.Answered(cid, &lnrpc.RPCMessage{MethodFullUri: cancelURI, IsError: true})
	_, err = e.call(sendURI, sendReq(r))
	mustAllow(t, err)
}

// An answer whose method is not the request's (an id reused after lnd
// restarted) is not taken for it.
func TestAnswerOfAnotherMethodIgnored(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(101, 1_000_000, 101)
	id, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	b, _ := proto.Marshal(&lnrpc.SendCoinsResponse{Txid: strings.Repeat("aa", 32)})
	e.p.Answered(id, &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/GetInfo", Serialized: b})
	_, hash := preimage(101)
	if l := e.p.Ledger.Lockups[hex.EncodeToString(hash)]; l.State != LockApproved {
		t.Fatalf("an answer of another method moved the lockup to %s", l.State)
	}
	e.p.ResetPending()
	if _, ok := e.p.pendingFor(id); ok {
		t.Fatal("pending kept after a new registration")
	}
}

// The race itself: a lockup checked before a cancel is allowed, recorded
// after it, is refused at the last step under the lock.
func TestLockupCheckedBeforeCancelRefused(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(102, 1_000_000, 102)
	want, _ := e.p.addrScript(r.LockupAddress)
	l, err := e.p.checkReverse(context.Background(), r, want, 1000)
	mustAllow(t, err)
	_, hash := preimage(102)
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: hash})
	mustAllow(t, err)
	err = e.p.allowSend(context.Background(), 999, l, 1000, 2)
	mustRefuse(t, err, "no longer open")
}

// An approval is never dropped for lack of evidence: lnd's wallet showing
// nothing (or an address spelt differently) keeps the reserve, the daily
// share and the block on cancelling. Only proof frees it: lnd released the
// HTLCs, the timeout is long past and the chain has no output to the script.
func TestApprovalNeverDroppedOnAbsence(t *testing.T) {
	e := newEnv(t)
	e.p.ApprovedExpireBlocks = 72
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	r, _, _ := e.reverse(103, 1_000_000, 103)
	_, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	_, hash := preimage(103)
	h := hex.EncodeToString(hash)
	at = at.Add(24 * time.Hour)
	e.p.Tick(context.Background())
	if l := e.p.Ledger.Lockups[h]; l == nil || l.State != LockApproved {
		t.Fatal("an approval was dropped because lnd showed nothing")
	}
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: hash})
	mustRefuse(t, err, "stays until")
	// HTLCs released, timeout long past, a decoy (not lnd's) paying the
	// script: the chain is scanned a step per round, never all at once
	e.ln.invoices[h].State = InvoiceCanceled
	e.chain.height = 1300
	pk, _ := e.p.addrScript(r.LockupAddress)
	e.chain.txs[strings.Repeat("77", 32)] = []Output{{PkScript: pk, Sat: r.OnchainAmount, Confirmations: 50}}
	e.p.Tick(context.Background())
	if l := e.p.Ledger.Lockups[h]; l.State != LockApproved {
		t.Fatalf("given up before the chain was scanned to the tip: %s", l.State)
	}
	for i := 0; i < 5; i++ {
		e.p.Tick(context.Background())
	}
	if l := e.p.Ledger.Lockups[h]; l.State != LockExpired {
		t.Fatalf("a decoy output kept the approval (or the proof never completed): %s", l.State)
	}
	// every block from the approval to the tip was read, none skipped
	next := int64(-1)
	for _, sc := range e.chain.scans {
		if sc[1]-sc[0]+1 > ScanStep {
			t.Fatalf("a scan read %d blocks at once", sc[1]-sc[0]+1)
		}
		if next >= 0 && sc[0] > next {
			t.Fatalf("blocks %d..%d skipped", next, sc[0]-1)
		}
		next = max(next, sc[1]+1)
	}
	if next <= 1300 {
		t.Fatalf("scan stopped at %d", next)
	}
}

// An approval whose lockup lnd sent (but whose answer was lost) is found on
// chain and followed, even far from the approval height.
func TestApprovalFoundWhenLndSentIt(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	r, _, _ := e.reverse(104, 1_000_000, 104)
	_, err := e.call(sendURI, sendReq(r)) // lnd's answer never comes back
	mustAllow(t, err)
	_, hash := preimage(104)
	h := hex.EncodeToString(hash)
	if l := e.p.Ledger.Lockups[h]; l == nil {
		t.Fatal("no approval")
	}
	at = at.Add(time.Hour)
	pk, _ := e.p.addrScript(r.LockupAddress)
	txid := strings.Repeat("78", 32)
	e.chain.height = 1290
	e.chain.txs[txid] = []Output{{PkScript: pk, Sat: r.OnchainAmount, Confirmations: 2}}
	e.ln.walletSends = append(e.ln.walletSends, walletSend{txid: txid, address: "not-decodable"})
	for i := 0; i < 4; i++ {
		e.p.Tick(context.Background())
	}
	if l := e.p.Ledger.Lockups[h]; l.State == LockApproved || l.TxID != txid {
		t.Fatalf("lnd's own lockup not found: %s %s", l.State, l.TxID)
	}
}

// Non-canonical (uppercase) addresses are refused, in the request and in the
// swap's row.
func TestUppercaseAddressRefused(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(106, 1_000_000, 106)
	req := sendReq(r)
	req.Addr = strings.ToUpper(req.Addr)
	_, err := e.call(sendURI, req)
	mustRefuse(t, err, "lowercase")
	up := r
	up.LockupAddress = strings.ToUpper(r.LockupAddress)
	want, _ := e.p.addrScript(r.LockupAddress)
	_, err = e.p.checkReverse(context.Background(), up, want, 1000)
	mustRefuse(t, err, "canonical")
}

// GetTransactions only bounded: recent start, a page at a time.
func TestGetTransactionsBounded(t *testing.T) {
	e := newEnv(t)
	const uri = "/lnrpc.Lightning/GetTransactions"
	e.chain.height = 10_000
	_, err := e.call(uri, &lnrpc.GetTransactionsRequest{StartHeight: 0, EndHeight: -1, MaxTransactions: 0})
	mustRefuse(t, err, "max_transactions")
	_, err = e.call(uri, &lnrpc.GetTransactionsRequest{StartHeight: 0, EndHeight: -1, MaxTransactions: 7})
	mustRefuse(t, err, "too old")
	_, err = e.call(uri, &lnrpc.GetTransactionsRequest{StartHeight: 9000, EndHeight: -1, MaxTransactions: 8})
	mustRefuse(t, err, "max_transactions")
	_, err = e.call(uri, &lnrpc.GetTransactionsRequest{StartHeight: 9000, EndHeight: -1, MaxTransactions: 7})
	mustAllow(t, err)
}

// The guard puts its own nonce in a hold invoice's memo (it replaces the
// request), and a 'creating' hold is taken as ours only with that memo.
func TestHoldMemoNonce(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	_, h := preimage(159)
	id, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h, Value: 50_000, Expiry: 3600, Memo: "theirs"})
	mustAllow(t, err)
	b := e.p.Replacement(id)
	var r lnrpc.AddHoldInvoiceRequest
	if b == nil || proto.Unmarshal(b, &r) != nil || !strings.HasPrefix(r.Memo, "kiloswap ") || !bytes.Equal(r.Hash, h) {
		t.Fatalf("no memo replacement: %v", r.Memo)
	}
	hold := e.p.Ledger.Holds[hex.EncodeToString(h)]
	if hold.Memo != r.Memo {
		t.Fatal("the memo was not recorded")
	}
	e.p.ResetPending()
	// same hash and amount, made then, but without the guard's memo
	e.ln.invoices[hex.EncodeToString(h)] = &InvoiceInfo{State: InvoiceOpen, ValueMsat: 50_000_000, Created: at, Memo: "theirs"}
	e.p.Tick(context.Background())
	if e.p.Ledger.Holds[hex.EncodeToString(h)] != nil {
		t.Fatal("an invoice without the guard's memo was taken for its hold")
	}
}

// Within the window the approval waits for lnd; a lockup lnd's wallet shows
// (unconfirmed) is followed.
func TestApprovedLockupFoundUnconfirmedInWallet(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	r, _, _ := e.reverse(104, 1_000_000, 104)
	_, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	at = at.Add(3 * time.Minute)
	e.ln.walletSends = append(e.ln.walletSends, walletSend{txid: strings.Repeat("ab", 32), address: r.LockupAddress, sat: r.OnchainAmount, vout: 0})
	e.p.Tick(context.Background())
	_, hash := preimage(104)
	if l := e.p.Ledger.Lockups[hex.EncodeToString(hash)]; l.State != LockSent || l.TxID != strings.Repeat("ab", 32) {
		t.Fatalf("not followed: %+v", l)
	}
}

// Splitting one lockup's worth into many small outputs does not make them
// shallow-safe: the depth asked is that of all payments not deep yet.
func TestDepthByExposureDefeatsSplitting(t *testing.T) {
	e := newEnv(t)
	inv, _, _ := e.submarine(110, 900_000, 1_000_000, 110)
	_, err := e.call(payURI, payReq(inv))
	mustAllow(t, err) // 1M not deep: 3 confirmations do
	inv, s2, _ := e.submarine(111, 900_000, 1_000_000, 111)
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "6 needed for 2000000")
	// once the first lockup is deep (12), the second stands alone again
	for id := range e.chain.txs {
		if id != s2.LockupTxID {
			e.chain.confs[id] = 12
		}
	}
	e.chain.height++ // depth changes with a new block (the guard caches per block)
	_, err = e.call(payURI, payReq(inv))
	mustAllow(t, err)
}

func TestInflightPaymentsCapped(t *testing.T) {
	e := newEnv(t)
	e.p.MaxInflightPayments = 1
	inv, _, _ := e.submarine(112, 100_000, 200_000, 112)
	_, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	inv, _, _ = e.submarine(113, 100_000, 200_000, 113)
	_, err = e.call(payURI, payReq(inv))
	mustRefuse(t, err, "not claimed yet")
}

func TestMaxSubmarineTimeout(t *testing.T) {
	e := newEnv(t)
	e.p.MaxSubTimeoutBlocks = 50
	inv, _, _ := e.submarine(114, 100_000, 200_000, 114)
	_, err := e.call(payURI, payReq(inv))
	mustRefuse(t, err, "at most 50")
}

// A big reverse lockup's refund cancels the hold invoice only at the depth
// its amount asks; near the HTLCs' deadline the refund pays what it takes.
func TestRefundFinalityByAmountAndUrgency(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(115, 50_000_000, 115) // tier: 12 confirmations
	txid, pk := e.sent(r, 115)
	r2, _, _ := e.reverse(116, 1_000_000, 116)
	txid2, pk2 := e.sent(r2, 116)
	e.chain.height = 1100
	e.p.Tick(context.Background())
	var first *wire.MsgTx
	for _, tx := range e.chain.broadcast {
		if tx.TxIn[0].PreviousOutPoint.Hash.String() == txid {
			first = tx
		}
	}
	op := txid + ":1"
	_, hash := preimage(115)
	e.chain.spenders[op] = &Spend{TxID: first.TxHash().String(), Witness: first.TxIn[0].Witness, Confirmations: 3}
	e.p.Tick(context.Background())
	if e.ln.invoices[hex.EncodeToString(hash)].State == InvoiceCanceled {
		t.Fatal("cancelled a 50M-sat lockup's invoice at 3 confirmations")
	}
	e.chain.spenders[op].Confirmations = 12
	e.p.Tick(context.Background())
	if e.ln.invoices[hex.EncodeToString(hash)].State != InvoiceCanceled {
		t.Fatal("not cancelled at 12")
	}
	// urgency: the other refund stuck past timeout + 18
	_ = pk
	n := len(e.chain.broadcast)
	e.chain.height = 1100 + 18
	e.p.Tick(context.Background())
	if len(e.chain.broadcast) != n+1 {
		t.Fatal("an urgent refund was not replaced")
	}
	last := e.chain.broadcast[len(e.chain.broadcast)-1]
	if last.TxIn[0].PreviousOutPoint.Hash.String() != txid2 {
		t.Fatal("the replacement is not the stuck refund's")
	}
	verify(t, last, pk2, r2.OnchainAmount)
	vsize := int64((last.SerializeSizeStripped()*3 + last.SerializeSize() + 3) / 4)
	if rate := float64(feeOf(last, r2.OnchainAmount)) / float64(vsize); rate < float64(e.p.MaxFeeRate) {
		t.Fatalf("urgent refund at %.1f sat/vB", rate)
	}
}

// AddInvoice is judged: bounded amount, expiry and rate, no preimage of the
// backend's choosing; its records are capped.
func TestAddInvoiceJudged(t *testing.T) {
	e := newEnv(t)
	const uri = "/lnrpc.Lightning/AddInvoice"
	_, err := e.call(uri, &lnrpc.AddInvoiceCheck{Value: 1000, Expiry: 3600})
	mustAllow(t, err)
	_, err = e.call(uri, &lnrpc.AddInvoiceCheck{Value: 1000, Expiry: 10 * 86400})
	mustRefuse(t, err, "expiry")
	_, err = e.call(uri, &lnrpc.AddInvoiceCheck{Value: 1000, RPreimage: make([]byte, 32)})
	mustRefuse(t, err, "preimage")
	for i := 0; i < MaxInvoicesPerMinute; i++ {
		e.call(uri, &lnrpc.AddInvoiceCheck{Value: 1})
	}
	_, err = e.call(uri, &lnrpc.AddInvoiceCheck{Value: 1})
	mustRefuse(t, err, "a minute")
	// the records are capped, oldest first
	led, _ := OpenLedger(filepath.Join(t.TempDir(), "l.json"), nil)
	for i := 0; i < MaxInvoices+100; i++ {
		led.Invoices[fmt.Sprintf("%064x", i)] = time.Now().Add(time.Duration(i) * time.Second)
	}
	led.Lock()
	led.MarkDirty()
	led.Unlock()
	if err := led.SaveIfDirty(); err != nil || len(led.Invoices) != MaxInvoices {
		t.Fatalf("invoices %d, %v", len(led.Invoices), err)
	}
	if _, ok := led.Invoices[fmt.Sprintf("%064x", 0)]; ok {
		t.Fatal("the oldest invoice was kept")
	}
}

func TestHeavyCallsRateLimited(t *testing.T) {
	e := newEnv(t)
	_, err := e.call("/lnrpc.Lightning/DescribeGraph", &lnrpc.WalletBalanceRequest{})
	mustRefuse(t, err, "not allowed") // the backend has no use for the whole graph
	for i := 0; i < 60; i++ {
		_, err := e.call("/lnrpc.Lightning/GetTransactions", &lnrpc.GetTransactionsRequest{StartHeight: 900, EndHeight: -1, MaxTransactions: 7})
		mustAllow(t, err)
	}
	_, err = e.call("/lnrpc.Lightning/GetTransactions", &lnrpc.GetTransactionsRequest{StartHeight: 900, EndHeight: -1, MaxTransactions: 7})
	mustRefuse(t, err, "a minute")
}

// A claim broadcast before a crash and never recorded is recognised (only
// our key spends the claim leaf) instead of failing every round.
func TestUnrecordedClaimRecovered(t *testing.T) {
	e := newEnv(t)
	inv, s, pre := e.submarine(117, 100_000, 200_000, 117)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: hex.EncodeToString(pre)}, false)
	if _, _, err := e.p.Claim(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	tx := e.chain.broadcast[0]
	// the crash: the ledger lost the claim
	x := e.p.Ledger.Payments[s.PreimageHash]
	x.State, x.ClaimTx, x.Bump = PaySucceeded, "", Bump{}
	txid, _, err := e.p.Claim(context.Background(), s.ID)
	if err != nil || txid != tx.TxHash().String() || len(e.chain.broadcast) != 1 {
		t.Fatalf("not recovered: %v %s (%d broadcasts)", err, txid, len(e.chain.broadcast))
	}
}

// Likewise an unrecorded refund of a reverse lockup.
func TestUnrecordedRefundRecovered(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(118, 1_000_000, 118)
	txid, _ := e.sent(r, 118)
	e.chain.height = 1100
	e.p.Tick(context.Background())
	_, hash := preimage(118)
	l := e.p.Ledger.Lockups[hex.EncodeToString(hash)]
	l.State, l.RefundTx, l.Bump = LockSent, "", Bump{}
	n := len(e.chain.broadcast)
	e.chain.spenders[txid+":1"].Confirmations = 3
	e.p.Tick(context.Background())
	if l.State != LockRefunded || len(e.chain.broadcast) != n {
		t.Fatalf("not recovered: %s, %d broadcasts", l.State, len(e.chain.broadcast)-n)
	}
}

// A hold invoice of ours holding too many HTLCs is cancelled.
func TestHoldWithTooManyPartsCancelled(t *testing.T) {
	e := newEnv(t)
	e.reverse(119, 1_000_000, 119)
	_, hash := preimage(119)
	inv := e.ln.invoices[hex.EncodeToString(hash)]
	inv.HTLCExpiries = make([]int32, 20)
	e.p.Tick(context.Background())
	if inv.State != InvoiceCanceled {
		t.Fatal("a hold invoice with 20 HTLCs was kept")
	}
}

// The API's refund only for a lockup on chain; slots expire for unpaid holds.
func TestRefundStateAndSlots(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(120, 1_000_000, 120)
	_, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	e.chain.height = 1200
	if _, _, err := e.p.Refund(context.Background(), r.ID); err == nil || !strings.Contains(err.Error(), "approved") {
		t.Fatalf("refunded a lockup not on chain: %v", err)
	}
	e2 := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e2.p.Now = func() time.Time { return at }
	e2.p.HoldSlotTime = 15 * time.Minute
	for i := byte(0); i < 5; i++ {
		_, hash := preimage(130 + i)
		id, err := e2.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 50_000})
		mustAllow(t, err)
		e2.answer(id, &lnrpc.AddHoldInvoiceResp{PaymentRequest: "x"}, false)
	}
	_, hash := preimage(140)
	_, err = e2.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 50_000})
	mustRefuse(t, err, "pending")
	at = at.Add(20 * time.Minute)
	_, err = e2.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Expiry: 3600, Value: 50_000})
	mustAllow(t, err)
}

// ---- the fourth review ----

// A lockup checked while the invoice was held is refused if, under the lock,
// lnd no longer holds it paid (a stale look must not let it through).
func TestAllowSendLooksAgainUnderTheLock(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(150, 1_000_000, 150)
	want, _ := e.p.addrScript(r.LockupAddress)
	l, err := e.p.checkReverse(context.Background(), r, want, 1000)
	mustAllow(t, err)
	_, hash := preimage(150)
	e.ln.invoices[hex.EncodeToString(hash)].State = InvoiceCanceled
	err = e.p.allowSend(context.Background(), 1, l, 1000, 2)
	mustRefuse(t, err, "no longer paid")
}

// A hold is recorded before lnd is told yes; a lost answer leaves it known,
// and lnd's own record settles it.
func TestHoldRecordedBeforeAnswer(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	_, h1 := preimage(151)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h1, Expiry: 3600, Value: 50_000})
	mustAllow(t, err)
	_, h2 := preimage(152)
	_, err = e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h2, Expiry: 3600, Value: 50_000})
	mustAllow(t, err)
	if e.p.Ledger.Holds[hex.EncodeToString(h1)].State != HoldCreating {
		t.Fatal("not recorded before the answer")
	}
	e.p.ResetPending() // the stream reset: both answers lost
	e.ln.invoices[hex.EncodeToString(h1)] = &InvoiceInfo{State: InvoiceOpen, ValueMsat: 50_000_000, Created: at, Memo: e.p.Ledger.Holds[hex.EncodeToString(h1)].Memo}
	at = at.Add(3 * time.Minute)
	e.p.Tick(context.Background())
	if x := e.p.Ledger.Holds[hex.EncodeToString(h1)]; x == nil || x.State != "open" {
		t.Fatal("a hold lnd made was not taken as open")
	}
	if e.p.Ledger.Holds[hex.EncodeToString(h2)] != nil {
		t.Fatal("a hold lnd never made stays recorded")
	}
}

// A payment lnd has no record of goes back after ten minutes even while
// its stream looks open (it must not fill the in-flight limit for good).
func TestUnknownToLndPaymentReleased(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	inv, s, _ := e.submarine(153, 100_000, 200_000, 153)
	_, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	at = at.Add(11 * time.Minute)
	e.p.Tick(context.Background())
	if x := e.p.Ledger.Payments[s.PreimageHash]; x.State != PayFailed {
		t.Fatalf("still %s", x.State)
	}
}

// Refund finality by the sum of refunds not final: two 6M refunds ask the
// depth of 12M (12 confirmations), not 6M's 6.
func TestRefundFinalityByExposure(t *testing.T) {
	e := newEnv(t)
	r1, _, _ := e.reverse(154, 6_000_000, 154)
	t1, _ := e.sent(r1, 154)
	r2, _, _ := e.reverse(155, 6_000_000, 155)
	e.sent(r2, 155)
	e.chain.height = 1100
	e.p.Tick(context.Background())
	op := t1 + ":1"
	e.chain.spenders[op].Confirmations = 6
	e.p.Tick(context.Background())
	_, h1 := preimage(154)
	if e.ln.invoices[hex.EncodeToString(h1)].State == InvoiceCanceled {
		t.Fatal("cancelled at 6 confirmations with 12M of refunds not final")
	}
	e.chain.spenders[op].Confirmations = 12
	e.p.Tick(context.Background())
	if e.ln.invoices[hex.EncodeToString(h1)].State != InvoiceCanceled {
		t.Fatal("not cancelled at 12")
	}
}

// A lockup whose invoice was settled but never claimed on chain is refunded
// after the grace (the coins would stay locked for good).
func TestSettledUnclaimedRefundedAfterGrace(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(156, 1_000_000, 156)
	e.sent(r, 156)
	_, hash := preimage(156)
	l := e.p.Ledger.Lockups[hex.EncodeToString(hash)]
	l.State = LockSettled
	e.chain.height = 1100 + settledGrace - 1
	e.p.Tick(context.Background())
	if len(e.chain.broadcast) != 0 {
		t.Fatal("refunded before the grace")
	}
	e.chain.height++
	e.p.Tick(context.Background())
	if len(e.chain.broadcast) != 1 || l.State != LockRefunding {
		t.Fatalf("not refunded after the grace: %s", l.State)
	}
}

func TestHoldExpiryCapped(t *testing.T) {
	e := newEnv(t)
	_, hash := preimage(157)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Value: 50_000, Expiry: 86400})
	mustRefuse(t, err, "expiry")
	_, err = e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Value: 50_000})
	mustRefuse(t, err, "expiry")
}

// A lockup paying under the node's estimate is refused (it would sit past
// its timeout); lnd answering an error does not drop the approval: lnd's
// wallet history decides.
func TestLockupFeeFloorAndErrorKeepsApproval(t *testing.T) {
	e := newEnv(t)
	e.chain.feeRate = 10
	r, _, _ := e.reverse(105, 1_000_000, 105)
	_, err := e.call(sendURI, sendReq(r)) // 2 sat/vB
	mustRefuse(t, err, "under")
	e.chain.feeRate = 2
	id, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	e.answer(id, nil, true)
	_, hash := preimage(105)
	if l := e.p.Ledger.Lockups[hex.EncodeToString(hash)]; l == nil || l.State != LockApproved {
		t.Fatal("an error answer dropped the approval before lnd's history was asked")
	}
}

// A 'creating' hold whose hash lnd knows from another invoice (another
// service's: other amount or older) is not taken as ours.
func TestCreatingHoldNotConfusedWithForeignInvoice(t *testing.T) {
	e := newEnv(t)
	at := time.Unix(1_800_000_000, 0)
	e.p.Now = func() time.Time { return at }
	_, h := preimage(158)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h, Value: 50_000, Expiry: 3600})
	mustAllow(t, err)
	e.p.ResetPending()
	e.ln.invoices[hex.EncodeToString(h)] = &InvoiceInfo{State: InvoiceAccepted, ValueMsat: 7_000_000, Created: at.Add(-time.Hour), Memo: e.p.Ledger.Holds[hex.EncodeToString(h)].Memo}
	e.p.Tick(context.Background())
	if e.p.Ledger.Holds[hex.EncodeToString(h)] != nil {
		t.Fatal("another service's invoice taken for our hold")
	}
}
