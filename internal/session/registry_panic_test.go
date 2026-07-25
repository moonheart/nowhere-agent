package session

import (
	"context"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
)

// panicProvider panics synchronously inside Stream, simulating a bug in the
// provider or loop internals that would otherwise crash the run worker goroutine
// (and, with it, every other tenant's run in the process).
type panicProvider struct{}

func (panicProvider) Name() string { return "panic" }
func (panicProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	panic("provider boom")
}

// TestSubmitRecoversFromPanickingProvider verifies the run worker recovers from
// a panic in the loop, settles the run failed, and emits an error event instead
// of taking down the process.
func TestSubmitRecoversFromPanickingProvider(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: registryLoop(panicProvider{})})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunFailed {
		t.Errorf("final status = %v want failed", got)
	}
	if got := len(eventsOfKind(t, rt, run.ID, string(agent.KindError))); got < 1 {
		t.Errorf("error events = %d want >=1", got)
	}
}
