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
	loop.Use(&OverflowMW{})
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
	loop := New(op, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	loop.Use(&OverflowMW{MaxRetries: 3})
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

// midStreamOverflowProvider streams a text delta and THEN fails with a context
// overflow — the shape the overflow fallback must NOT retry: the delta already
// reached the client, and a retry would re-emit it.
type midStreamOverflowProvider struct{ calls int }

func (p *midStreamOverflowProvider) Name() string { return "midstream-overflow" }
func (p *midStreamOverflowProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	p.calls++
	ch := make(chan provider.Event, 4)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "partial"}
	ch <- provider.Event{Type: provider.EventError, Err: &provider.ContextOverflowError{StatusCode: 413, Body: "request too large"}}
	close(ch)
	return ch, nil
}

func TestLoopMidStreamOverflowNotRetried(t *testing.T) {
	p := &midStreamOverflowProvider{}
	emit := &memEmitter{}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	loop.Use(&OverflowMW{})

	_, err := loop.Run(context.Background(), bigConversation(4, 50), emit)
	if err == nil {
		t.Fatal("a mid-stream overflow should fail the run, not be retried")
	}
	if p.calls != 1 {
		t.Errorf("calls = %d, want 1 (mid-stream failures must not retry — deltas already reached the client)", p.calls)
	}
	if emit.count(KindText) != 1 {
		t.Errorf("text frames = %d, want 1 (a retry would duplicate the streamed delta)", emit.count(KindText))
	}
}
