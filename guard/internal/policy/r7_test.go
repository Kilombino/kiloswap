package policy

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"github.com/Kilombino/kiloswap/guard/internal/swapscript"
)

// A hold whose AddHoldInvoice answer was lost (stream reset) stays
// 'creating': the backend can neither cancel nor read another service's
// invoice created later with that hash.
func TestR7CancelForeignAfterLostAnswer(t *testing.T) {
	e := newEnv(t)
	_, h := preimage(90)
	_, err := e.call(holdURI, &lnrpc.AddHoldInvoiceRequest{Hash: h, Expiry: 3600, Value: 20000, CltvExpiry: 5})
	mustAllow(t, err)
	e.p.ResetPending() // stream reset: lnd's error answer never seen
	e.ln.invoices[hex.EncodeToString(h)] = &InvoiceInfo{Memo: "bridge", State: InvoiceAccepted, PaidMsat: 5e9, ValueMsat: 5e9, Created: time.Now()}
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: h})
	mustRefuse(t, err, "not the guard's")
	_, err = e.call(lookupURI, &lnrpc.PaymentHash{RHash: h})
	if err == nil {
		t.Fatal("lookup of a foreign invoice allowed")
	}
	// nor once it reads 'open' by mistake: the memo is checked against lnd
	e.p.Ledger.Lock()
	e.p.Ledger.Holds[hex.EncodeToString(h)].State = "open"
	e.p.Ledger.Unlock()
	_, err = e.call(cancelURI, &lnrpc.CancelInvoiceMsg{PaymentHash: h})
	mustRefuse(t, err, "not the guard's")
}

// SendCoins is accepted only in its canonical form (lnd refusing a field
// after the guard approved would leave an approval never executed).
func TestR7SendCoinsCanonical(t *testing.T) {
	e := newEnv(t)
	r, _, _ := e.reverse(91, 1_000_000, 91)
	for name, mod := range map[string]func(*lnrpc.SendCoinsRequest){
		"label":    func(q *lnrpc.SendCoinsRequest) { q.Label = strings.Repeat("x", 600) },
		"minconfs": func(q *lnrpc.SendCoinsRequest) { q.MinConfs = 1000 },
		"strategy": func(q *lnrpc.SendCoinsRequest) { q.CoinSelectionStrategy = 3 },
	} {
		q := sendReq(r)
		mod(q)
		if _, err := e.call(sendURI, q); err == nil {
			t.Fatalf("%s: a non-canonical SendCoins was allowed", name)
		}
	}
}

type slowChain struct {
	*fakeChain
	calls int
	delay time.Duration
}

func (c *slowChain) FindOutputs(ctx context.Context, pk []byte, sat int64, from, to int64) ([]Found, error) {
	c.calls++
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.fakeChain.FindOutputs(ctx, pk, sat, from, to)
}

// Approvals lnd never executed, each with a decoy output, neither starve the
// real lockups' work (that comes first) nor stay forever (decoys are not
// lnd's); and at most MaxUnresolved of them can exist.
func TestR7DecoyedApprovalsStarveSettlement(t *testing.T) {
	e := newEnv(t)
	sc := &slowChain{fakeChain: e.chain, delay: 30 * time.Millisecond}
	e.p.Chain = sc
	e.p.ApprovedExpireBlocks = 72
	e.p.MaxPendingReverse = 10
	e.p.MaxLockupsPerBlock = 0 // this test is about the tick, not the rate
	now := time.Now()
	e.p.Now = func() time.Time { return now }
	var hashes []string
	for i := byte(0); i < 5; i++ {
		r, _, _ := e.reverse(uint32(100+i), 20000, 100+i)
		id, err := e.call(sendURI, sendReq(r))
		mustAllow(t, err)
		e.answer(id, nil, true)
		pk, _ := e.p.addrScript(r.LockupAddress)
		e.chain.txs["decoy"+string(rune('a'+i))] = []Output{{PkScript: pk, Sat: 20000, Confirmations: 2}}
		_, h := preimage(100 + i)
		hashes = append(hashes, hex.EncodeToString(h))
	}
	for _, h := range hashes {
		if l := e.p.Ledger.Lockups[h]; l == nil || l.State != LockApproved {
			t.Fatal("setup: the decoyed approval is not held")
		}
	}
	r, pre, user := e.reverse(120, 1_000_000, 120)
	txid, _ := e.sent(r, 120)
	_, hash := preimage(120)
	server, _ := e.keys.Public(120)
	out, _ := swapscript.Build(swapscript.Params{Kind: swapscript.Reverse, Server: server, User: user.PubKey(), PaymentHash: hash, Timeout: 1100})
	cb, _ := out.ControlBlock(out.ClaimLeaf)
	e.chain.spent[txid+":1"] = true
	e.chain.spenders[txid+":1"] = &Spend{TxID: "claim", Witness: [][]byte{make([]byte, 64), pre, out.ClaimLeaf.Script, cb}, Confirmations: 6}

	now = now.Add(time.Hour)
	e.chain.height = 1300
	for _, h := range hashes {
		if inv := e.ln.invoices[h]; inv != nil {
			inv.State = InvoiceCanceled
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond) // a short tick
	e.p.Tick(ctx)
	cancel()
	settled := false
	for _, p := range e.ln.settled {
		if bytes.Equal(p, pre) {
			settled = true
		}
	}
	if !settled {
		t.Fatalf("the claimed lockup was not settled in a short tick (%d chain searches first)", sc.calls)
	}
	for i := 0; i < 20; i++ {
		e.p.Tick(context.Background())
	}
	for _, h := range hashes {
		if l := e.p.Ledger.Lockups[h]; l != nil && l.State == LockApproved {
			t.Fatalf("a decoyed approval never expired")
		}
	}
}

// At most MaxUnresolved lockups may be allowed and not seen on chain.
func TestR7UnresolvedCapped(t *testing.T) {
	e := newEnv(t)
	e.p.MaxUnresolved = 2
	e.p.MaxPendingReverse = 10
	for i := byte(0); i < 3; i++ {
		r, _, _ := e.reverse(uint32(130+i), 20000, 130+i)
		_, err := e.call(sendURI, sendReq(r))
		if i < 2 {
			mustAllow(t, err)
		} else {
			mustRefuse(t, err, "not seen on chain yet")
		}
	}
}
