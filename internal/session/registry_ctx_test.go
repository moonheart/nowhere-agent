package session

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"nowhere-agent/internal/observability"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/reqctx"
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

// TestSubmitRunCtxInheritsValuesNotCancellation pins the reqctx.Detach split:
// the run context inherits the submitter's VALUES (request id, request-scoped
// logger) but not its CANCELLATION — the run must outlive the connection. It is
// the handoff test for the submit path; attach and resume funnel through the
// same Submit boundary (attach reads the broker, resume re-Submits), so this one
// pins the typed handoff all three paths rely on.
func TestSubmitRunCtxInheritsValuesNotCancellation(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	p := &ctxCaptureProvider{seen: make(chan context.Context, 1)}

	reqLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	reqCtx, cancel := context.WithCancel(context.Background())
	reqCtx = reqctx.WithRequestID(reqCtx, "req-trace-42")
	reqCtx = reqctx.WithLogger(reqCtx, reqLog)
	reqCtx = reqctx.WithSessionID(reqCtx, sess.ID)
	if _, err := rg.Submit(reqCtx, sess.ID, RunWork{Loop: registryLoop(p)}); err != nil {
		t.Fatal(err)
	}
	cancel() // the submitting connection goes away immediately

	runCtx := <-p.seen
	if reqctx.RequestID(runCtx) != "req-trace-42" {
		t.Errorf("run ctx lost request id: got %q want req-trace-42", reqctx.RequestID(runCtx))
	}
	// execute() derives a run-scoped logger (adding session/run attrs) from
	// whatever LoggerFromContext yields, falling back to slog.Default only when
	// the handoff dropped it. So the check is: the run logger was derived from
	// the request logger, not silently replaced by the process default.
	runLog := observability.LoggerFromContext(runCtx)
	if runLog == nil {
		t.Error("run ctx lost the request-scoped logger (reqctx.Detach handoff broken)")
	}
	if runLog == slog.Default() {
		t.Error("run ctx fell back to the process default logger — reqctx.Detach did not thread the request-scoped logger through")
	}
	if reqctx.SessionID(runCtx) != sess.ID {
		t.Errorf("run ctx lost session id: got %q want %q", reqctx.SessionID(runCtx), sess.ID)
	}
	if runCtx.Err() != nil {
		t.Errorf("run ctx was cancelled by the submitter disconnecting: %v", runCtx.Err())
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Errorf("run ended %v, want done — the disconnect must not kill the run", got)
	}
}

type ctxKeyForTest struct{}
