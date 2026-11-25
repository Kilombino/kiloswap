package sources

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Paid requests count in full; quotes waiting for payment only until they
// expire, and anonymous (unsigned) ones only up to a cap.
func TestLSPReserved(t *testing.T) {
	now := time.Now().Unix()
	f := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(f, []byte(fmt.Sprintf(`{"orders":{
	 "a":{"sats":10000000,"status":"paid"},
	 "b":{"sats":5000000,"status":"waiting_payment","expires":%d},
	 "c":{"sats":50000000,"status":"waiting_payment","expires":%d},
	 "d":{"sats":15000000,"status":"waiting_payment","expires":%d,"unsigned":true},
	 "e":{"sats":15000000,"status":"waiting_payment","expires":%d,"unsigned":true},
	 "f":{"sats":9000000,"status":"open"}}}`, now+600, now-10, now+600, now+600)), 0o600)
	got, err := LSP{Path: f}.Reserved()
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(10_000_000 + 5_000_000 + MaxAnonymousReserve); got != want {
		t.Fatalf("reserved %d, want %d", got, want)
	}
}
