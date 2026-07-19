package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// overflowProvider rejects the first failTimes calls with a context-overflow
// error, then answers with text — simulating a provider that accepts the
// request once the loop has shrunk the view.
type overflowProvider struct {
	mu        sync.Mutex
	calls     int
	failTimes int
	sizes     []int // messages length per call
}

func (p *overflowProvider) Name() string { return "overflow" }

func (p *overflowProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	p.calls++
	p.sizes = append(p.sizes, len(req.Messages))
	fail := p.calls <= p.failTimes
	p.mu.Unlock()
	if fail {
		return nil, &provider.ContextOverflowError{StatusCode: 413, Body: "request too large"}
	}
	ch := make(chan provider.Event, 5)
	for _, e := range textResponse("recovered") {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestLoopRetriesOnContextOverflow(t *testing.T) {
	op := &overflowProvider{failTimes: 2}
	loop := New(op, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	history := bigConversation(8, 200) // plenty of rounds to drop

	out, err := loop.Run(context.Background(), history, &memEmitter{})
	if err != nil {
		t.Fatalf("overflow should be retried, not fail the run: %v", err)
	}
	if len(out) == 0 {
		t.Error("expected a produced message after recovery")
	}
	// 3 attempts total: 2 overflows + 1 success.
	if op.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 overflow + 1 success)", op.calls)
	}
	// Each retry sent a strictly smaller view.
	for i := 1; i < len(op.sizes); i++ {
		if op.sizes[i] >= op.sizes[i-1] {
			t.Errorf("view should shrink between retries: %v", op.sizes)
		}
	}
}

func TestLoopOverflowRetryBounded(t *testing.T) {
	op := &overflowProvider{failTimes: 100} // never accepts
	cfg := Config{Model: "m", MaxTokens: 100, MaxOverflowRetries: 3}
	loop := New(op, toolruntime.NewRegistry(), cfg)
	_, err := loop.Run(context.Background(), bigConversation(8, 200), &memEmitter{})
	if err == nil {
		t.Fatal("run should fail after exhausting overflow retries")
	}
	// 1 initial + 3 retries = 4 attempts max.
	if op.calls > 4 {
		t.Errorf("calls = %d, retry bound should cap at 4", op.calls)
	}
}

func TestLoopNonOverflowErrorFailsImmediately(t *testing.T) {
	op := &overflowProvider{failTimes: 0}
	// Swap in a provider that fails with a NON-overflow error.
	fail := &failProvider{err: errors.New("quota exceeded")}
	loop := New(fail, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	_, err := loop.Run(context.Background(), bigConversation(2, 50), &memEmitter{})
	if err == nil {
		t.Fatal("non-overflow error should fail the run")
	}
	if fail.calls != 1 {
		t.Errorf("non-overflow error should not be retried, calls = %d", fail.calls)
	}
	_ = op
}

type failProvider struct {
	err   error
	calls int
}

func (p *failProvider) Name() string { return "fail" }
func (p *failProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	p.calls++
	return nil, p.err
}
