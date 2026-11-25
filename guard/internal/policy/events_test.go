package policy

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Every event the guard emits has a Telegram message in the watcher (or is
// one it handles on purpose: refusals, the heartbeat).
func TestEveryEventReachesTheWatcher(t *testing.T) {
	src := map[string]bool{}
	re := regexp.MustCompile(`(?:notify|Lifecycle)\("([a-z_]+)"`)
	files, _ := filepath.Glob("../../internal/*/*.go")
	more, _ := filepath.Glob("../../cmd/*/*.go")
	for _, f := range append(files, more...) {
		if filepath.Ext(f) != ".go" || regexp.MustCompile(`_test\.go$`).MatchString(f) {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range re.FindAllSubmatch(b, -1) {
			src[string(m[1])] = true
		}
	}
	w, err := os.ReadFile("../../../watch/kiloswap-watch.mjs")
	if err != nil {
		t.Fatal(err)
	}
	handled := map[string]bool{"guard_refusal": true, "guard_alive": true}
	for ev := range src {
		if handled[ev] {
			continue
		}
		if !regexp.MustCompile(`\b` + ev + `: \(\) =>`).Match(w) {
			t.Errorf("the guard emits %q and the watcher has no message for it", ev)
		}
	}
	if len(src) < 20 {
		t.Fatalf("found only %d events: the scan is broken", len(src))
	}
}
