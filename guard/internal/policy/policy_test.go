package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
)

type fakeSwaps struct {
	reverse    []ReverseSwap
	swaps      []Swap
	pendingN   int
	pendingSat int64
	err        error
}

func (f *fakeSwaps) ReverseByLockup(_ context.Context, a string) ([]ReverseSwap, error) {
	var out []ReverseSwap
	for _, r := range f.reverse {
		if r.LockupAddress == a {
			out = append(out, r)
		}
	}
	return out, f.err
}
func (f *fakeSwaps) ReverseByHash(_ context.Context, h string) (bool, error) {
	for _, r := range f.reverse {
		if r.PreimageHash == h {
			return true, f.err
		}
	}
	return false, f.err
}
func (f *fakeSwaps) PendingReverse(context.Context) (int, int64, error) {
	return f.pendingN, f.pendingSat, f.err
}
func (f *fakeSwaps) SwapByInvoice(_ context.Context, inv string) (*Swap, error) {
	for _, s := range f.swaps {
		if strings.EqualFold(s.Invoice, inv) {
			s := s
			return &s, f.err
		}
	}
	return nil, f.err
}

type fakeWallet struct {
	b   Balance
	err error
}

func (w *fakeWallet) Balance(context.Context) (Balance, error) { return w.b, w.err }

type fakeInvoices struct {
	inv map[string][2]int64 // hash -> state, paid msat
	err error
}

func (f *fakeInvoices) Invoice(_ context.Context, h []byte) (int32, int64, error) {
	v, ok := f.inv[hex.EncodeToString(h)]
	if !ok {
		return 0, 0, f.errOr(errors.New("unable to locate invoice"))
	}
	return int32(v[0]), v[1], f.err
}
func (f *fakeInvoices) errOr(e error) error {
	if f.err != nil {
		return f.err
	}
	return e
}

type fakeChain struct {
	pays map[string][2]int64 // txid|address -> sat, confirmations
	err  error
}

func (f *fakeChain) PaidTo(_ context.Context, txid, addr string) (int64, int64, error) {
	v := f.pays[txid+"|"+addr]
	return v[0], v[1], f.err
}

type fakeLSP struct {
	sat int64
	err error
}

func (l *fakeLSP) Reserved() (int64, error) { return l.sat, l.err }

const btc = int64(100_000_000)

type rig struct {
	p      *Policy
	swaps  *fakeSwaps
	inv    *fakeInvoices
	chain  *fakeChain
	wallet *fakeWallet
	lsp    *fakeLSP
	now    time.Time
	alerts []string
	id     uint64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		swaps:  &fakeSwaps{},
		inv:    &fakeInvoices{inv: map[string][2]int64{}},
		chain:  &fakeChain{pays: map[string][2]int64{}},
		wallet: &fakeWallet{b: Balance{Confirmed: 5 * btc}},
		lsp:    &fakeLSP{},
		now:    time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
	}
	clock := func() time.Time { return r.now }
	led, err := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"), clock)
	if err != nil {
		t.Fatal(err)
	}
	r.p = &Policy{
		MaxPerOpSat: btc, DailySat: 2 * btc, ReserveSat: 50_000_000,
		MaxPendingReverse: 5, FeeRatio: 0.0001, MinFeeSat: 121, MaxFeeRate: 25,
		RequiredConfs: 3, Swaps: r.swaps, Invoices: r.inv, Chain: r.chain, Wallet: r.wallet, LSP: r.lsp, Ledger: led, Now: clock,
		Alert: func(m, why string) { r.alerts = append(r.alerts, m+": "+why) },
	}
	return r
}

func (r *rig) call(uri string, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return r.raw(uri, b)
}

func (r *rig) raw(uri string, b []byte) error {
	r.id++
	return r.p.Intercept(context.Background(), CaveatCondition, r.id,
		&lnrpc.RPCMessage{MethodFullUri: uri, Serialized: b})
}

func (r *rig) answer(isErr bool) {
	r.p.Answered(r.id, &lnrpc.RPCMessage{IsError: isErr})
}

func mustRefuse(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !Refused(err) {
		t.Fatalf("want a refusal containing %q, got %v", want, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q in %q", want, err.Error())
	}
}

func mustPass(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("want pass, got %v", err)
	}
}

const (
	sendCoins = "/lnrpc.Lightning/SendCoins"
	sendPay   = "/routerrpc.Router/SendPaymentV2"
	addHold   = "/invoicesrpc.Invoices/AddHoldInvoice"
	cancelInv = "/invoicesrpc.Invoices/CancelInvoice"
	settleInv = "/invoicesrpc.Invoices/SettleInvoice"
)

// rev adds a reverse swap whose hold invoice lnd holds as paid.
func (r *rig) rev(addr string, sat int64, status string) {
	h := sha256.Sum256([]byte(addr))
	hh := hex.EncodeToString(h[:])
	r.swaps.reverse = append(r.swaps.reverse, ReverseSwap{LockupAddress: addr, OnchainAmount: sat, Status: status, PreimageHash: hh})
	r.inv.inv[hh] = [2]int64{InvoiceAccepted, (sat + 300) * 1000}
}

func lockupRig(t *testing.T, sat int64) *rig {
	r := newRig(t)
	r.rev("bcrt1qlockup", sat, "swap.created")
	return r
}

// sub adds a submarine swap whose lockup the node shows confirmed.
func (r *rig) sub(invoice string, sat int64, status string) {
	r.swaps.swaps = append(r.swaps.swaps, Swap{Invoice: invoice, InvoiceAmount: sat, Status: status,
		LockupAddress: "bcrt1plock-" + invoice, LockupTxID: "tx-" + invoice})
	r.chain.pays["tx-"+invoice+"|bcrt1plock-"+invoice] = [2]int64{sat + 200, 3}
}

func TestCaveatAndMethods(t *testing.T) {
	r := newRig(t)
	r.id++
	err := r.p.Intercept(context.Background(), "v0", r.id, &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/GetInfo"})
	mustRefuse(t, err, "caveat condition")
	for _, uri := range []string{
		"/lnrpc.Lightning/OpenChannelSync", "/lnrpc.Lightning/OpenChannel",
		"/lnrpc.Lightning/CloseChannel", "/lnrpc.Lightning/SendMany",
		"/walletrpc.WalletKit/FundPsbt", "/walletrpc.WalletKit/SignPsbt",
		"/lnrpc.Lightning/BakeMacaroon", "/lnrpc.Lightning/SendPaymentSync",
		"/routerrpc.Router/SendToRouteV2", "/lnrpc.Lightning/UpdateChannelPolicy",
		"/walletrpc.WalletKit/SendOutputs", "/lnrpc.Lightning/StopDaemon",
	} {
		mustRefuse(t, r.call(uri, &lnrpc.CancelInvoiceMsg{}), "not allowed")
		mustRefuse(t, r.p.Stream(uri), "not allowed")
	}
	mustPass(t, r.call("/lnrpc.Lightning/GetInfo", &lnrpc.CancelInvoiceMsg{}))
	mustPass(t, r.p.Stream(sendPay))
	if len(r.alerts) == 0 {
		t.Fatal("refusals should alert")
	}
}

func TestSendCoinsOnlyToLockup(t *testing.T) {
	r := lockupRig(t, 1_000_000)
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qattacker", Amount: 1_000_000}), "no reverse swap")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_001}), "no reverse swap")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", SendAll: true}), "send_all")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000, SpendUnconfirmed: true}), "confirmed")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000, Outpoints: [][]byte{{1}}}), "chosen")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000, SatPerVbyte: 500}), "fee rate")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000, SatPerByte: 500}), "fee rate")
	r.swaps.reverse[0].Status = "transaction.mempool"
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "no reverse swap")
	r.swaps.reverse[0].Status = "swap.created"
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000, SatPerByte: 2, Label: "Lockup"}))
	r.answer(false)
	// a lockup is sent once
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "already sent")
}

func TestSendCoinsRefusedByLndMayRetry(t *testing.T) {
	r := lockupRig(t, 1_000_000)
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}))
	r.answer(true) // lnd failed it: nothing sent
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}))
}

func TestPerOperationAndDailyLimits(t *testing.T) {
	r := newRig(t)
	r.wallet.b.Confirmed = 10 * btc
	r.rev("a1", btc+1, "invoice.pending")
	r.rev("a2", btc, "invoice.pending")
	r.rev("a3", btc, "invoice.pending")
	r.rev("a4", btc, "invoice.pending")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a1", Amount: btc + 1}), "limit per operation")
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a2", Amount: btc}))
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a3", Amount: btc}))
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a4", Amount: btc}), "daily limit")
	r.now = r.now.Add(24*time.Hour + time.Second)
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a4", Amount: btc}))
}

func TestDailyLimitSurvivesRestart(t *testing.T) {
	r := newRig(t)
	r.wallet.b.Confirmed = 10 * btc
	r.rev("a2", btc, "invoice.pending")
	r.rev("a3", btc, "invoice.pending")
	r.rev("a4", 1, "invoice.pending")
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a2", Amount: btc}))
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a3", Amount: btc}))
	led, err := OpenLedger(r.p.Ledger.path, func() time.Time { return r.now })
	if err != nil {
		t.Fatal(err)
	}
	r.p.Ledger = led
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a4", Amount: 1}), "daily limit")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a2", Amount: btc}), "already sent")
}

func TestLSPReserve(t *testing.T) {
	r := lockupRig(t, 30_000_000)
	r.wallet.b = Balance{Confirmed: 100_000_000, Locked: 5_000_000, AnchorReserve: 10_000}
	r.lsp.sat = 20_000_000
	// 100M - 5M - 10k - 50M - 20M = 24.99M < 30M
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 30_000_000}), "LSP reserve")
	r.lsp.sat = 0
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 30_000_000}))
}

func TestFailClosedOnUnknownState(t *testing.T) {
	r := lockupRig(t, 1_000_000)
	r.lsp.err = errors.New("no state file")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "LSP requests unknown")
	r.lsp.err = nil
	r.wallet.err = errors.New("lnd down")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "wallet balance unknown")
	r.wallet.err = nil
	r.swaps.err = errors.New("db down")
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "swap database")
	mustRefuse(t, r.call(cancelInv, &lnrpc.CancelInvoiceMsg{PaymentHash: make([]byte, 32)}), "swap database")
}

func TestUnknownFieldsRefused(t *testing.T) {
	r := lockupRig(t, 1_000_000)
	b, _ := proto.Marshal(&lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000})
	b = protowire.AppendTag(b, 99, protowire.VarintType)
	b = protowire.AppendVarint(b, 1)
	mustRefuse(t, r.raw(sendCoins, b), "does not know")
	mustRefuse(t, r.raw(sendCoins, []byte{0xff, 0xff}), "unreadable")
}

const inv = "lnbcrt1000u1pexample"

func payRig(t *testing.T, sat int64, status string) *rig {
	r := newRig(t)
	r.sub(inv, sat, status)
	return r
}

func TestPaymentOnlySwapInvoices(t *testing.T) {
	r := payRig(t, 1_000_000, "transaction.confirmed")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "lnbcrt1attacker", FeeLimitSat: 100}), "not the invoice")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{Dest: []byte{2}, Amt: 1000, PaymentHash: make([]byte, 32)}), "only invoices")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, Amt: 5_000_000}), "own amount")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, Amp: true}), "AMP")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, DestCustomRecords: map[uint64][]byte{65536: {1}}}), "custom records")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, LastHopPubkey: []byte{2}}), "steered")
	// fee cap: max(1M * 0.0001, 121) = 121 sat... 1M sat * 1e-4 = 100 sat, floor 121
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 1000}), "routing fee")
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121, MaxParts: 5, TimeoutSeconds: 60}))
	// the same payment again (a retry) passes and counts once
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: strings.ToUpper(inv), FeeLimitSat: 121}))
	if got := r.p.Ledger.Since(r.now.Add(-time.Hour)); got != 1_000_000 {
		t.Fatalf("counted %d", got)
	}
}

func TestPaymentFeeScalesWithAmount(t *testing.T) {
	r := payRig(t, btc, "invoice.pending")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 10_001}), "routing fee")
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 10_000}))
}

func TestPaymentNotBeforeLockupConfirmed(t *testing.T) {
	for _, st := range []string{"swap.created", "invoice.set", "transaction.mempool", "invoice.failedToPay", "transaction.refunded"} {
		r := payRig(t, 1_000_000, st)
		mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121}), "not ready")
	}
}

func TestPaymentLimits(t *testing.T) {
	r := payRig(t, btc+1, "transaction.confirmed")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 20_000}), "limit per operation")
	r = newRig(t)
	r.sub("i1", btc, "transaction.confirmed")
	r.sub("i2", btc, "transaction.confirmed")
	r.sub("i3", 10_000, "transaction.confirmed")
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i1", FeeLimitSat: 10_000}))
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i2", FeeLimitSat: 10_000}))
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i3", FeeLimitSat: 121}), "daily limit")
}

func TestDailyLimitIsShared(t *testing.T) {
	r := newRig(t)
	r.wallet.b.Confirmed = 10 * btc
	r.sub("i1", btc, "transaction.confirmed")
	r.rev("a1", btc, "invoice.pending")
	r.rev("a2", 1, "invoice.pending")
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i1", FeeLimitSat: 10_000}))
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a1", Amount: btc}))
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "a2", Amount: 1}), "daily limit")
}

func TestHoldInvoices(t *testing.T) {
	r := newRig(t)
	h := sha256.Sum256([]byte("preimage-of-a-kiloswap-swap-0001"))
	mustRefuse(t, r.call(addHold, &lnrpc.AddHoldInvoiceRequest{Hash: []byte{1}, Value: 1000}), "32-byte")
	mustRefuse(t, r.call(addHold, &lnrpc.AddHoldInvoiceRequest{Hash: h[:], Value: btc + 1}), "not within")
	mustRefuse(t, r.call(addHold, &lnrpc.AddHoldInvoiceRequest{Hash: h[:], Value: 1, ValueMsat: 1000}), "one amount")
	r.swaps.pendingN = 5
	mustRefuse(t, r.call(addHold, &lnrpc.AddHoldInvoiceRequest{Hash: h[:], Value: 100_000}), "reverse swaps pending")
	r.swaps.pendingN = 4
	r.wallet.b.Confirmed = 2 * btc // 2 BTC - 0.5 reserve = 1.5 available
	r.swaps.pendingSat = btc
	mustRefuse(t, r.call(addHold, &lnrpc.AddHoldInvoiceRequest{Hash: h[:], Value: 60_000_000}), "not fit")
	mustPass(t, r.call(addHold, &lnrpc.AddHoldInvoiceRequest{Hash: h[:], ValueMsat: 40_000_000_000, Memo: "Send to BTC address"}))
	// its own invoice may be settled and cancelled
	pre := []byte("preimage-of-a-kiloswap-swap-0001")
	mustPass(t, r.call(settleInv, &lnrpc.SettleInvoiceMsg{Preimage: pre}))
	mustPass(t, r.call(cancelInv, &lnrpc.CancelInvoiceMsg{PaymentHash: h[:]}))
}

func TestOthersInvoicesUntouchable(t *testing.T) {
	r := newRig(t)
	bridge := sha256.Sum256([]byte("a hold invoice of the bridge...."))
	mustRefuse(t, r.call(cancelInv, &lnrpc.CancelInvoiceMsg{PaymentHash: bridge[:]}), "not a Kiloswap")
	mustRefuse(t, r.call(settleInv, &lnrpc.SettleInvoiceMsg{Preimage: []byte("a hold invoice of the bridge....")}), "not a Kiloswap")
	mustRefuse(t, r.call(settleInv, &lnrpc.SettleInvoiceMsg{Preimage: []byte("short")}), "32-byte")
	// one in the swap database (created before the guard kept a record) is its own
	r.swaps.reverse = []ReverseSwap{{PreimageHash: hex.EncodeToString(bridge[:])}}
	mustPass(t, r.call(cancelInv, &lnrpc.CancelInvoiceMsg{PaymentHash: bridge[:]}))
}

// The backend's database is its own word: a row it wrote is not enough.
func TestLockupNeedsLndToHoldThePayment(t *testing.T) {
	r := lockupRig(t, 1_000_000)
	h := r.swaps.reverse[0].PreimageHash
	for _, st := range []int64{0, 1, 2} { // OPEN, SETTLED, CANCELED
		r.inv.inv[h] = [2]int64{st, 2_000_000_000}
		mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "not paid and held")
	}
	r.inv.inv[h] = [2]int64{InvoiceAccepted, 999_999_999} // less than the lockup
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "less than")
	delete(r.inv.inv, h) // a row with no invoice in lnd at all
	mustRefuse(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}), "lnd invoice")
	r.inv.inv[h] = [2]int64{InvoiceAccepted, 1_000_000_000}
	mustPass(t, r.call(sendCoins, &lnrpc.SendCoinsRequest{Addr: "bcrt1qlockup", Amount: 1_000_000}))
}

func TestPaymentNeedsTheLockupOnChain(t *testing.T) {
	r := payRig(t, 1_000_000, "transaction.confirmed")
	key := "tx-" + inv + "|bcrt1plock-" + inv
	r.chain.pays[key] = [2]int64{1_000_200, 2}
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121}), "confirmations")
	r.chain.pays[key] = [2]int64{999_999, 6}
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121}), "less than")
	r.chain.pays[key] = [2]int64{0, 0} // a transaction the node does not know, or paying elsewhere
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121}), "confirmations")
	r.chain.err = errors.New("rpc down")
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121}), "node")
	r.chain.err = nil
	r.swaps.swaps[0].LockupTxID = ""
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: inv, FeeLimitSat: 121}), "no lockup")
}

func TestFailedPaymentFreesTheDailyLimit(t *testing.T) {
	r := newRig(t)
	r.sub("i1", btc, "invoice.pending")
	r.sub("i2", btc, "invoice.pending")
	r.sub("i3", btc, "invoice.pending")
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i1", FeeLimitSat: 10_000}))
	upd := func(status int32) {
		b, _ := proto.Marshal(&lnrpc.Payment{Status: status})
		r.p.Answered(r.id, &lnrpc.RPCMessage{StreamRpc: true, Serialized: b})
	}
	upd(1) // in flight: still counted
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i2", FeeLimitSat: 10_000}))
	upd(3) // i2 failed: it no longer counts
	mustPass(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i3", FeeLimitSat: 10_000}))
	upd(2) // i3 succeeded: counted
	if got := r.p.Ledger.Since(r.now.Add(-time.Hour)); got != 2*btc {
		t.Fatalf("counted %d, want i1 and i3", got)
	}
	// i2 again now passes only if the limit allows: it does not
	mustRefuse(t, r.call(sendPay, &lnrpc.SendPaymentRequest{PaymentRequest: "i2", FeeLimitSat: 10_000}), "daily limit")
}
