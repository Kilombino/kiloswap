package middleware

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kilombino/kiloswap/guard/internal/lnrpc"
)

type slowJudge struct {
	mu      sync.Mutex
	delay   time.Duration
	stalls  []string
	forgot  []uint64
	allowed bool
}

func (j *slowJudge) Stream(string) error { return nil }
func (j *slowJudge) Intercept(ctx context.Context, _ string, _ uint64, _ *lnrpc.RPCMessage) error {
	time.Sleep(j.delay) // a database query that ignores its context
	return nil
}
func (j *slowJudge) Answered(uint64, *lnrpc.RPCMessage) {}
func (j *slowJudge) Replacement(uint64) []byte          { return nil }
func (j *slowJudge) Stall(why string) {
	j.mu.Lock()
	j.stalls = append(j.stalls, why)
	j.mu.Unlock()
}
func (j *slowJudge) Epoch() uint64 { return 0 }
func (j *slowJudge) ForgetIn(id, _ uint64) {
	j.mu.Lock()
	j.forgot = append(j.forgot, id)
	j.mu.Unlock()
}

func request(id uint64) *lnrpc.RPCMiddlewareRequest {
	return &lnrpc.RPCMiddlewareRequest{RequestId: id, MsgId: id,
		InterceptType: &lnrpc.RPCMiddlewareRequest_Request{Request: &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/SendCoins"}}}
}

// A judgment that does not finish in time is refused (never a late yes),
// reported as a stall, and what it recorded is forgotten once it ends.
func TestSlowJudgmentRefusedAndForgotten(t *testing.T) {
	old := JudgeTimeout
	JudgeTimeout = 50 * time.Millisecond
	defer func() { JudgeTimeout = old }()
	j := &slowJudge{delay: 200 * time.Millisecond}
	start := time.Now()
	fb := judge(context.Background(), j, request(7), log.New(io.Discard, "", 0))
	if fb.Error == "" || !strings.Contains(fb.Error, "no judgment") {
		t.Fatalf("a late judgment was not refused: %q", fb.Error)
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatal("the answer waited for the slow judgment")
	}
	time.Sleep(300 * time.Millisecond)
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.stalls) != 1 || len(j.forgot) != 1 || j.forgot[0] != 7 {
		t.Fatalf("stalls %v forgot %v", j.stalls, j.forgot)
	}
}

// Judgments run side by side: ten slow ones take about one's time.
func TestJudgmentsConcurrent(t *testing.T) {
	j := &slowJudge{delay: 100 * time.Millisecond}
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if fb := judge(context.Background(), j, request(uint64(i)), log.New(io.Discard, "", 0)); fb.Error != "" {
				t.Errorf("refused: %s", fb.Error)
			}
		}(i)
	}
	wg.Wait()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("judgments queued: %v", d)
	}
}

// A request bigger than MaxRequestBytes is refused without being parsed.
func TestOversizedRequestRefused(t *testing.T) {
	j := &slowJudge{}
	req := &lnrpc.RPCMiddlewareRequest{RequestId: 9, MsgId: 9, InterceptType: &lnrpc.RPCMiddlewareRequest_Request{
		Request: &lnrpc.RPCMessage{MethodFullUri: "/lnrpc.Lightning/DecodePayReq", Serialized: make([]byte, MaxRequestBytes+1)}}}
	fb := judge(context.Background(), j, req, log.New(io.Discard, "", 0))
	if !strings.Contains(fb.Error, "over") {
		t.Fatalf("not refused: %q", fb.Error)
	}
}
