package policy

import (
	"context"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/swapscript"
	"google.golang.org/protobuf/proto"
)

// M1: no reverse swap under MinReverseSat, no lockup under half of it.
func TestV8MinReverse(t *testing.T) {
	e := newEnv(t)
	_, h := preimage(200)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h, Expiry: 3600, Value: 330})
	mustRefuse(t, err, "minimum of a reverse swap")
	r, _, _ := e.reverse(201, 20_000, 201)
	q := sendReq(r)
	q.Amount = 330
	_, err = e.call(sendURI, q)
	mustRefuse(t, err, "minimum of a lockup")
}

// M1: the hold invoice must pay for the lockup, its fee and a refund's at
// the lockup's own fee rate.
func TestV8LockupCoverage(t *testing.T) {
	e := newEnv(t)
	e.chain.feeRate = 6
	r, _, _ := e.reverse(202, 1_000_000, 202) // pays 2000 sat over the lockup
	q := sendReq(r)
	q.SatPerByte = 7 // 266*7+330 = 2192 > 2000
	_, err := e.call(sendURI, q)
	mustRefuse(t, err, "a refund's")
	q.SatPerByte = 6 // 1596+330 = 1926 <= 2000
	_, err = e.call(sendURI, q)
	mustAllow(t, err)
}

// M1: a loop of reverse swaps never claimed gets at most MaxLockupsPerBlock
// lockups a block.
func TestV8LockupsPerBlock(t *testing.T) {
	e := newEnv(t)
	e.p.MaxPendingReverse = 20
	for i := byte(0); i < 3; i++ {
		r, _, _ := e.reverse(uint32(210+i), 20_000, 210+i)
		id, err := e.call(sendURI, sendReq(r))
		if i < 2 {
			mustAllow(t, err)
			e.answer(id, &lnrpc.SendCoinsResponse{Txid: strings.Repeat("d"+string(rune('0'+i)), 32)}, false)
		} else {
			mustRefuse(t, err, "a block")
		}
	}
	e.chain.height++
	r, _, _ := e.reverse(219, 20_000, 219)
	_, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
}

type countingChain struct {
	*fakeChain
	spenders atomic.Int32
}

func (c *countingChain) Spender(ctx context.Context, txid string, vout uint32, from int64) (*Spend, error) {
	c.spenders.Add(1)
	return c.fakeChain.Spender(ctx, txid, vout, from)
}

// A paid lockup spent by the user's refund is remembered (one chain search,
// one alert), and claimed again if a reorg undoes the refund.
func TestV8PaymentLostRemembered(t *testing.T) {
	e := newEnv(t)
	cc := &countingChain{fakeChain: e.chain}
	e.p.Chain = cc
	inv, s, pre := e.submarine(220, 100_000, 200_000, 220)
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	e.answer(id, &lnrpc.Payment{Status: paymentSucceeded, PaymentPreimage: hex.EncodeToString(pre)}, false)
	op := s.LockupTxID + ":0"
	e.chain.spent[op] = true
	e.chain.spenders[op] = &Spend{TxID: strings.Repeat("ee", 32), Confirmations: 1}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		e.p.Tick(ctx)
	}
	x := e.p.Ledger.Payments[s.PreimageHash]
	if x.State != PayLost || x.LostTx != strings.Repeat("ee", 32) {
		t.Fatalf("not remembered as lost: %s", x.State)
	}
	if n := cc.spenders.Load(); n != 1 {
		t.Fatalf("searched the chain %d times", n)
	}
	lost := 0
	for _, n := range e.notes {
		if strings.Contains(n, "guard_payment_lost") {
			lost++
		}
	}
	if lost != 1 {
		t.Fatalf("%d lost alerts", lost)
	}
	// a reorg: the lockup is unspent again
	delete(e.chain.spent, op)
	delete(e.chain.spenders, op)
	e.p.Tick(ctx)
	if x.State != PayClaimed || len(e.chain.broadcast) != 1 {
		t.Fatalf("not claimed after the reorg: %s", x.State)
	}
}

// The backend may not settle a hold whose lockup the guard is refunding.
func TestV8NoSettleWhileRefunding(t *testing.T) {
	e := newEnv(t)
	r, pre, _ := e.reverse(221, 1_000_000, 221)
	e.sent(r, 221)
	e.chain.height = 1100
	e.p.Tick(context.Background())
	_, hash := preimage(221)
	if l := e.p.Ledger.Lockups[hex.EncodeToString(hash)]; l.State != LockRefunding {
		t.Fatalf("not refunding: %s", l.State)
	}
	_, err := e.call("/invoicesrpc.Invoices/SettleInvoice", &lnrpc.SettleInvoiceMsg{Preimage: pre})
	mustRefuse(t, err, "only from a claim on chain")
}

// A replacement made for a request that is then refused, or judged too
// late, is never kept.
func TestV8ReplacementsFreed(t *testing.T) {
	e := newEnv(t)
	_, h := preimage(222)
	id := uint64(77777)
	if err := e.p.Intercept(context.Background(), CaveatCondition, id, e.msg(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h, Expiry: 3600, Value: 20000})); err != nil {
		t.Fatal(err)
	}
	e.p.ForgetIn(id, e.p.Epoch())
	if b := e.p.Replacement(id); b != nil {
		t.Fatal("a late judgment's replacement was kept")
	}
	_, h2 := preimage(223)
	id2 := uint64(77778)
	_ = e.p.Intercept(context.Background(), CaveatCondition, id2, e.msg(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h2, Expiry: 3600, Value: 20000}))
	e.p.ResetPending()
	if b := e.p.Replacement(id2); b != nil {
		t.Fatal("a replacement survived lnd's restart")
	}
}

// R: off by default (the request kept); on, the fee limit is lowered to the
// lockup's surplus (never under the floor, never over FeeRatio).
func TestV8RoutingCapFlag(t *testing.T) {
	e := newEnv(t)
	inv, _, _ := e.submarine(224, 100_000, 100_400, 224) // surplus 400 - claim(302) = 98
	id, err := e.call(payURI, payReq(inv))
	mustAllow(t, err)
	if b := e.p.Replacement(id); b != nil {
		t.Fatal("the fee limit replaced with the cap off")
	}
	e.p.RoutingCap, e.p.RoutingFloorSat = true, 50
	inv, _, _ = e.submarine(225, 100_000, 100_400, 225)
	id, err = e.call(payURI, payReq(inv))
	mustAllow(t, err)
	var q lnrpc.SendPaymentRequest
	if err := proto.Unmarshal(e.p.Replacement(id), &q); err != nil {
		t.Fatal(err)
	}
	if q.FeeLimitSat != 0 || q.FeeLimitMsat != 98_000 || q.PaymentRequest != inv {
		t.Fatalf("replaced with %d sat / %d msat", q.FeeLimitSat, q.FeeLimitMsat)
	}
	e.p.RoutingFloorSat = 110 // the floor wins over a smaller surplus
	inv, _, _ = e.submarine(226, 100_000, 100_400, 226)
	id, _ = e.call(payURI, payReq(inv))
	_ = proto.Unmarshal(e.p.Replacement(id), &q)
	if q.FeeLimitMsat != 110_000 {
		t.Fatalf("floor: %d msat", q.FeeLimitMsat)
	}
	// a limit far over the cap is lowered, not refused
	inv, _, _ = e.submarine(227, 100_000, 100_400, 227)
	q5 := payReq(inv)
	q5.FeeLimitSat = 5000
	id, err = e.call(payURI, q5)
	mustAllow(t, err)
	_ = proto.Unmarshal(e.p.Replacement(id), &q)
	if q.FeeLimitMsat != 110_000 || q.FeeLimitSat != 0 {
		t.Fatalf("5000 sat asked: replaced with %d msat", q.FeeLimitMsat)
	}
	// and a payment that then fails gives its caps back only on lnd's FAILED
	e.answer(id, &lnrpc.Payment{Status: paymentInFlight}, false)
	x := e.p.Ledger.Payments[e.swaps.swaps[inv].PreimageHash]
	if x.State != PayInflight {
		t.Fatalf("state %s", x.State)
	}
	e.answer(id, nil, true) // the stream ends: unknown, nothing released
	if x.State != PayUnknown || e.p.Ledger.Outpoints[x.LockupTx+":0"] == "" {
		t.Fatalf("released without lnd's FAILED: %s", x.State)
	}
	_ = time.Second
	_ = swapscript.Reverse
}
