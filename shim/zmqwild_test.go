package main

import (
	"strings"
	"testing"
)

func TestZMQWildcard(t *testing.T) {
	in := []byte(`{"result":[{"type":"pubrawblock","address":"tcp://192.168.0.14:28332","hwm":20},{"type":"pubrawtx","address":"tcp://192.168.0.14:28333","hwm":20}],"error":null,"id":1}`)
	out := string(zmqWildcard(in))
	if strings.Contains(out, "192.168") || !strings.Contains(out, "tcp://0.0.0.0:28332") || !strings.Contains(out, "tcp://0.0.0.0:28333") || !strings.Contains(out, `"hwm":20`) {
		t.Fatalf("rewritten: %s", out)
	}
}
