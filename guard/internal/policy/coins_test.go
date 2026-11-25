package policy

import (
	"strings"
	"testing"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func coin(b byte, sat int64) Utxo {
	return Utxo{TxID: strings.Repeat(string("0123456789abcdef"[b%16]), 64), Vout: uint32(b), Sat: sat, Confs: 3, Type: 4}
}

func TestPickCoins(t *testing.T) {
	big := coin(1, 346_000_000)
	smalls := []Utxo{coin(2, 5_000), coin(3, 30_000), coin(4, 12_000), coin(5, 200_000)}
	all := append([]Utxo{big}, smalls...)
	// the smallest coin that covers 9,123 sat and its fee: 12,000
	in := pickCoins(all, 9_123, 2)
	if len(in) != 1 || in[0].Sat != 12_000 {
		t.Fatalf("picked %v", in)
	}
	// 220,000: no single small coin; 200,000 + 30,000 (fewest, largest first)
	in = pickCoins(all, 220_000, 2)
	if len(in) != 2 || in[0].Sat != 200_000 || in[1].Sat != 30_000 {
		t.Fatalf("picked %v", in)
	}
	// only the big coin pays: lnd's own choice
	if in := pickCoins(all, 1_000_000, 2); in != nil {
		t.Fatalf("picked %v instead of leaving the big coin to lnd", in)
	}
	// one coin only: lnd's choice
	if in := pickCoins([]Utxo{big}, 9_123, 2); in != nil {
		t.Fatalf("picked %v", in)
	}
	// the fee counts: 12,000 does not cover 11,900 at 10 sat/vB
	in = pickCoins(all, 11_900, 10)
	if len(in) != 1 || in[0].Sat != 30_000 {
		t.Fatalf("picked %v", in)
	}
}

func outpointsOf(t *testing.T, b []byte) []string {
	var q lnrpc.SendCoinsRequest
	if err := proto.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, raw := range q.Outpoints {
		var op lnrpc.OutPoint
		if err := proto.Unmarshal(raw, &op); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op.TxidStr+":"+string(rune('0'+op.OutputIndex)))
	}
	return ops
}

// A lockup spends a small coin, not the wallet's big one; a coin given to
// one lockup is not given to the next; with only the big coin, lnd chooses.
func TestLockupCoinsChosen(t *testing.T) {
	e := newEnv(t)
	e.p.MaxPendingReverse = 10
	e.ln.utxos = []Utxo{coin(1, 346_000_000), coin(2, 50_000), coin(3, 60_000)}
	r, _, _ := e.reverse(240, 20_000, 240)
	id, err := e.call(sendURI, sendReq(r))
	mustAllow(t, err)
	b := e.p.Replacement(id)
	if b == nil {
		t.Fatal("the lockup's coins were not named")
	}
	ops := outpointsOf(t, b)
	if len(ops) != 1 || !strings.HasSuffix(ops[0], ":2") {
		t.Fatalf("spends %v, not the 50,000 sat coin", ops)
	}
	var q lnrpc.SendCoinsRequest
	_ = proto.Unmarshal(b, &q)
	if q.Addr != r.LockupAddress || q.Amount != r.OnchainAmount || q.SatPerByte != 2 {
		t.Fatal("the rewrite changed more than the coins")
	}
	e.answer(id, &lnrpc.SendCoinsResponse{Txid: strings.Repeat("71", 32)}, false)
	r2, _, _ := e.reverse(241, 20_000, 241)
	id, err = e.call(sendURI, sendReq(r2))
	mustAllow(t, err)
	ops = outpointsOf(t, e.p.Replacement(id))
	if len(ops) != 1 || !strings.HasSuffix(ops[0], ":3") {
		t.Fatalf("second lockup spends %v, not the other small coin", ops)
	}
	e.answer(id, &lnrpc.SendCoinsResponse{Txid: strings.Repeat("72", 32)}, false)
	// both small coins given out: the third lockup is lnd's to choose
	e.chain.height++
	r3, _, _ := e.reverse(242, 20_000, 242)
	id, err = e.call(sendURI, sendReq(r3))
	mustAllow(t, err)
	if b := e.p.Replacement(id); b != nil {
		t.Fatalf("named %v with only the big coin left", outpointsOf(t, b))
	}
	// unconfirmed coins are never named
	e.ln.utxos = []Utxo{coin(1, 346_000_000), {TxID: strings.Repeat("e", 64), Vout: 0, Sat: 50_000, Confs: 0, Type: 4}}
	e.chain.height++
	r4, _, _ := e.reverse(243, 20_000, 243)
	id, err = e.call(sendURI, sendReq(r4))
	mustAllow(t, err)
	if b := e.p.Replacement(id); b != nil {
		t.Fatal("named an unconfirmed coin")
	}
	_ = protowire.BytesType
}
