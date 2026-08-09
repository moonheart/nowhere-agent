package session

import (
	"context"
	"testing"

	"nowhere-agent/internal/observability"
	"nowhere-agent/internal/provider"
)

// ctxCaptureProvider records the context its Stream call receives, so tests can
// assert what the run context carried into the provider layer.
type ctxCaptureProvider struct {
	seen chan context.Context
}

func (p *ctxCaptureProvider) Name() string { return "ctx-capture" }

func (p *ctxCaptureProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	p.seen <- ctx
	ch := make(chan provider.Event, 5)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "hi"}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	close(ch)
	return ch, nil
}

// TestSubmitRunCtxInheritsValuesNotCancellation pins the WithoutCancel split:
// the run context inherits the submitter's VALUES (request id, request-scoped
// logger) but not its CANCELLATION — the run must outlive the connection.
func TestSubmitRunCtxInheritsValuesNotCancellation(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	p := &ctxCaptureProvider{seen: make(chan context.Context, 1)}

	reqCtx, cancel := context.WithCancel(context.Background())
	reqCtx = context.WithValue(reqCtx, ctxKeyForTest{}, "trace-value")
	if _, err := rg.Submit(reqCtx, sess.ID, RunWork{Loop: registryLoop(p)}); err != nil {
		t.Fatal(err)
	}
	cancel() // the submitting connection goes away immediately

	runCtx := <-p.seen
	if got := runCtx.Value(ctxKeyForTest{}); got != "trace-value" {
		t.Errorf("run ctx lost submitter value: got %v", got)
	}
	if runCtx.Err() != nil {
		t.Errorf("run ctx was cancelled by the submitter disconnecting: %v", runCtx.Err())
	}
	if observability.LoggerFromContext(runCtx) == nil {
		t.Error("run ctx missing the run-scoped logger")
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Errorf("run ended %v, want done — the disconnect must not kill the run", got)
	}
}

type ctxKeyForTest struct{}
