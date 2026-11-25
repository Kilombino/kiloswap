package policy

import "testing"

// Kilombino's production tiers (25 Nov 2026): 1 conf up to 0.01 BTC of
// shallow exposure, 2 up to 0.1 BTC, 3 above (never more than 3).
func TestProductionConfTiers(t *testing.T) {
	p := &Policy{RequiredConfs: 1, ConfTiers: []ConfTier{{1_000_000, 1}, {10_000_000, 2}, {100_000_000, 3}}}
	for _, c := range []struct{ sat, want int64 }{
		{10_000, 1}, {1_000_000, 1}, {1_000_001, 2}, {10_000_000, 2}, {10_000_001, 3}, {100_000_000, 3}, {200_000_000, 3},
	} {
		if got := p.confsFor(c.sat); got != c.want {
			t.Fatalf("confsFor(%d) = %d, want %d", c.sat, got, c.want)
		}
	}
}
