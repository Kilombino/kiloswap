package policy

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Coin selection for lockups. lnd left to itself spends whatever coin it
// likes, often the wallet's one big coin: until the next block the whole of
// it is unconfirmed change, the confirmed balance drops to almost nothing,
// and the LSP, the reserve and the next swaps all see an empty wallet. The
// guard names the coins instead (SendCoins' outpoints, by replacing the
// request): the smallest confirmed coin that covers the lockup and its fee,
// or else the smaller coins smallest first (the lockup consolidates dust,
// up to MaxLockupInputs), or else the fewest of them largest first, and the
// biggest coin only when nothing else can pay (lnd's own choice then, as
// before). A coin worth less than what spending it costs is left alone.

// MaxLockupInputs is the most coins one lockup may gather.
const MaxLockupInputs = 10

// coinChosenFor is how long a coin given to a lockup is not given to
// another (lnd leases it once it builds the transaction).
const coinChosenFor = 10 * time.Minute

// inputVbytes is what spending a coin of lnd's address type weighs.
func inputVbytes(t int32) float64 {
	switch t {
	case 0:
		return 68 // p2wkh
	case 1:
		return 91 // np2wkh
	default:
		return 58 // p2tr
	}
}

// lockupFee is a lockup's fee at rate with these inputs: a P2TR lockup
// output and a P2TR change output, and some slack so lnd never finds the
// coins short.
func lockupFee(rate float64, in []Utxo) int64 {
	vb := 11.0 + 43 + 43 + 20
	for _, u := range in {
		vb += inputVbytes(u.Type)
	}
	return int64(math.Ceil(vb * rate))
}

// pickCoins chooses the coins for amount at rate: nil means let lnd choose
// (only the biggest coin can pay, or nothing can).
func pickCoins(coins []Utxo, amount int64, rate float64) []Utxo {
	if len(coins) < 2 {
		return nil
	}
	sorted := append([]Utxo(nil), coins...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Sat < sorted[j].Sat })
	biggest := sorted[len(sorted)-1]
	others := sorted[:len(sorted)-1]
	// the smallest single coin that pays, other than the biggest
	for _, u := range others {
		if u.Sat >= amount+lockupFee(rate, []Utxo{u}) {
			return []Utxo{u}
		}
	}
	// coins that pay more than they cost to spend
	var worth []Utxo
	for _, u := range others {
		if float64(u.Sat) > inputVbytes(u.Type)*rate {
			worth = append(worth, u)
		}
	}
	// the smaller coins smallest first: the lockup gathers the dust
	var in []Utxo
	var sum int64
	for i := 0; i < len(worth) && len(in) < MaxLockupInputs; i++ {
		in = append(in, worth[i])
		sum += worth[i].Sat
		if sum >= amount+lockupFee(rate, in) {
			return in
		}
	}
	// too many small ones for MaxLockupInputs: the fewest, largest first
	in, sum = nil, 0
	for i := len(worth) - 1; i >= 0 && len(in) < MaxLockupInputs; i-- {
		in = append(in, worth[i])
		sum += worth[i].Sat
		if sum >= amount+lockupFee(rate, in) {
			return in
		}
	}
	_ = biggest
	return nil
}

// chooseCoins names the lockup's coins by replacing the SendCoins request.
// When lnd cannot list its coins, or only the biggest one pays, the
// request is left as it is.
func (p *Policy) chooseCoins(ctx context.Context, requestID uint64, r *lnrpc.SendCoinsRequest, rate float64) error {
	minConfs := max(int32(1), r.MinConfs)
	coins, err := p.LN.ListUnspent(ctx, minConfs)
	if err != nil {
		return nil // lnd's own choice, as before
	}
	now := p.now()
	p.pmu.Lock()
	if p.chosen == nil {
		p.chosen = map[string]time.Time{}
	}
	var free []Utxo
	for _, u := range coins {
		op := fmt.Sprintf("%s:%d", u.TxID, u.Vout)
		if at, ok := p.chosen[op]; ok && now.Sub(at) < coinChosenFor {
			continue
		}
		if u.Confs >= int64(minConfs) && u.Sat > 0 {
			free = append(free, u)
		}
	}
	p.pmu.Unlock()
	in := pickCoins(free, r.Amount, rate)
	if in == nil {
		return nil
	}
	repl := proto.Clone(r).(*lnrpc.SendCoinsRequest)
	repl.Outpoints = nil
	for _, u := range in {
		var b []byte
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, u.TxID)
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(u.Vout))
		repl.Outpoints = append(repl.Outpoints, b)
	}
	b, err := proto.Marshal(repl)
	if err != nil {
		return refuse("the request cannot be rewritten safely: %v", err)
	}
	// only the coins changed
	var back lnrpc.SendCoinsRequest
	if proto.Unmarshal(b, &back) != nil || back.Addr != r.Addr || back.Amount != r.Amount || back.SendAll ||
		back.SatPerVbyte != r.SatPerVbyte || back.SatPerByte != r.SatPerByte || back.TargetConf != r.TargetConf ||
		back.Label != r.Label || back.MinConfs != r.MinConfs || back.SpendUnconfirmed ||
		back.CoinSelectionStrategy != r.CoinSelectionStrategy || len(back.Outpoints) != len(in) {
		return refuse("the request cannot be rewritten safely")
	}
	p.pmu.Lock()
	for _, u := range in {
		p.chosen[fmt.Sprintf("%s:%d", u.TxID, u.Vout)] = now
	}
	for op, at := range p.chosen {
		if now.Sub(at) >= coinChosenFor {
			delete(p.chosen, op)
		}
	}
	p.pmu.Unlock()
	p.setReplacement(requestID, b)
	return nil
}
