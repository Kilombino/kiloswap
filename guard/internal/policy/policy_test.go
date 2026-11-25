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
	mu       sync.Mutex
	invoices map[string]*InvoiceInfo
	payreqs  map[string]*PayReq
	tracks   map[string][2]string // hash -> status, preimage
	settled  [][]byte
	balance  Balance
	addr     string
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
func (f *fakeLN) TrackPayment(_ context.Context, h []byte) (int32, string, error) {
	t, ok := f.tracks[hex.EncodeToString(h)]
	if !ok {
		return 0, "", nil
	}
	var s int32
	fmt.Sscan(t[0], &s)
	return s, t[1], nil
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

type fakeChain struct {
	mu        sync.Mutex
	txs       map[string][]Output
	spent     map[string]bool
	spenders  map[string]*Spend
	height    int64
	broadcast []*wire.MsgTx
	confs     map[string]int64
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
func (c *fakeChain) FeeRate(context.Context) (float64, error) { return 2, nil }
func (c *fakeChain) Broadcast(_ context.Context, raw string) (string, error) {
	b, _ := hex.DecodeString(raw)
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.broadcast = append(c.broadcast, &tx)
	c.mu.Unlock()
	return tx.TxHash().String(), nil
}
func (c *fakeChain) Confirmations(_ context.Context, txid string) (int64, error) {
	return c.confs[txid], nil
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
	r.CltvLimit = 95 // timeout 1100, height 1000, safety 6
	_, err = e.call(payURI, r)
	mustRefuse(t, err, "no time to claim")
	r = payReq(inv)
	r.OutgoingChanIds = []uint64{1}
	_, err = e.call(payURI, r)
	mustRefuse(t, err, "outgoing")
	r = payReq(inv)
	r.FeeLimitSat = 5000
	_, err = e.call(payURI, r)
	mustRefuse(t, err, "fee limit")
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
	id, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Value: sat + 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	e.answer(id, &lnrpc.AddHoldInvoiceResp{PaymentRequest: "lnbcrt"}, false)
	e.ln.invoices[hex.EncodeToString(hash)] = &InvoiceInfo{State: InvoiceAccepted, PaidMsat: (sat + 1000) * 1000, HTLCExpiries: []int32{1200}}
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
	_, err = e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Value: 1_501_000})
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
	_, err = e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: other, Value: 1000})
	mustRefuse(t, err, "already exists")
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: other})
	mustRefuse(t, err, "not a Kiloswap")
}

func TestHoldRecordedOnlyWhenLndCreatedIt(t *testing.T) {
	e := newEnv(t)
	_, hash := preimage(40)
	id, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Value: 1000})
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
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: hash, Value: 1000})
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
	e.chain.spenders[op] = &Spend{TxID: first.TxHash().String(), Witness: first.TxIn[0].Witness, Confirmations: 1}
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
	e.chain.spenders[op] = &Spend{TxID: first.TxHash().String(), Witness: first.TxIn[0].Witness, Confirmations: 1}
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
