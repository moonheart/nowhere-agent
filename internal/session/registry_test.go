package session

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// stubProvider drives a run with a fixed set of text deltas, optionally waiting
// on a gate before finishing so a test can act mid-run.
type stubProvider struct {
	deltas []string
	gate   <-chan struct{} // if non-nil, blocks until closed before finishing
}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 16)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	go func() {
		defer close(ch)
		if p.gate != nil {
			select {
			case <-ctx.Done():
				return
			case <-p.gate:
			}
		}
		for _, d := range p.deltas {
			select {
			case <-ctx.Done():
				return
			case ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: d}:
			}
		}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	}()
	return ch, nil
}

func registryLoop(p provider.Adapter) *agent.Loop {
	return agent.New(p, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100})
}

func newRegistrySession(t *testing.T) (*Runtime, *RunRegistry, Session) {
	t.Helper()
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	rg := NewRunRegistry(rt, rt.Bus())
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}
	return rt, rg, sess
}

// waitSettle polls until the session has no active run or the deadline passes.
func waitSettle(t *testing.T, rt *Runtime, sessionID string) RunStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, active, _ := rt.ActiveRun(context.Background(), sessionID); !active {
			runs, _ := rt.RunsForSession(context.Background(), sessionID)
			if len(runs) > 0 {
				return runs[len(runs)-1].Status
			}
			return ""
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("run did not settle")
	return ""
}

func eventsOfKind(t *testing.T, rt *Runtime, runID string, kind string) []Event {
	t.Helper()
	evs, err := rt.Replay(context.Background(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestSubmitRunsToCompletion(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	// Subscribe to the live broker before the run so we capture content frames as
	// they stream (the broker clears retained frames on settle, so we collect live).
	contentCh, unsub := rt.Broker().Subscribe(sess.ID, 64)
	defer unsub()

	run, err := rg.Submit(context.Background(), sess.ID, RunWork{
		Loop: registryLoop(&stubProvider{deltas: []string{"hello", " world"}}),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Errorf("final status = %v want done", got)
	}
	// Content deltas go to the live broker, not the durable log; lifecycle (done)
	// stays in run_events. (redis-stream-live)
	textCount := 0
	drain := true
	for drain {
		select {
		case e := <-contentCh:
			if e.Kind == string(agent.KindText) {
				textCount++
			}
		default:
			drain = false
		}
	}
	if textCount != 2 {
		t.Errorf("broker text frames = %d want 2", textCount)
	}
	if got := len(eventsOfKind(t, rt, run.ID, string(agent.KindDone))); got != 1 {
		t.Errorf("done events = %d want 1", got)
	}
	// No content deltas in the durable log.
	if got := len(eventsOfKind(t, rt, run.ID, string(agent.KindText))); got != 0 {
		t.Errorf("run_events text rows = %d want 0 (content lives in the broker/messages)", got)
	}
}

func TestSubmitEnforcesSingleActiveRun(t *testing.T) {
	_, rg, sess := newRegistrySession(t)
	gate := make(chan struct{})
	defer close(gate)
	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: registryLoop(&stubProvider{gate: gate})}); err != nil {
		t.Fatal(err)
	}
	// Give the worker a moment to register.
	time.Sleep(20 * time.Millisecond)
	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: registryLoop(&stubProvider{})}); err != ErrRunActive {
		t.Errorf("second submit = %v want ErrRunActive", err)
	}
}

// TestCancelIsTransportIndependent is the core of the refactor: a run is
// cancelled via the registry with no HTTP request involved, and it settles
// cancelled with a terminal cancelled event published to the durable log.
func TestCancelIsTransportIndependent(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	gate := make(chan struct{}) // never closed: the run blocks until cancelled
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: registryLoop(&stubProvider{gate: gate})})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // let the worker park on the gate

	if !rg.Cancel(sess.ID) {
		t.Fatal("Cancel returned false, want true for an active run")
	}
	if got := waitSettle(t, rt, sess.ID); got != RunCancelled {
		t.Errorf("final status = %v want cancelled", got)
	}
	if got := len(eventsOfKind(t, rt, run.ID, string(agent.KindCancelled))); got != 1 {
		t.Errorf("cancelled events = %d want 1", got)
	}
}

func TestCancelWithNoActiveRun(t *testing.T) {
	_, rg, sess := newRegistrySession(t)
	if rg.Cancel(sess.ID) {
		t.Error("Cancel returned true with no active run, want false")
	}
}

// TestTerminalEventPrecedesSettle is the regression for the attach-side race: an
// observer replaying the run after it reports inactive must still find the
// terminal event in the durable log (persisted before settle).
func TestTerminalEventPrecedesSettle(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: registryLoop(&stubProvider{deltas: []string{"x"}})})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the run to report inactive; the terminal event must already be
	// queryable from the durable log at that point.
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Fatalf("final status = %v want done", got)
	}
	if got := len(eventsOfKind(t, rt, run.ID, string(agent.KindDone))); got != 1 {
		t.Errorf("done event missing from durable log after settle; got %d want 1", got)
	}
}

// TestSubscriberSeesLiveEvents verifies a bus subscriber attached to the session
// receives the run's events as they are published.
func TestSubscriberSeesLiveEvents(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	ch, unsub := rt.Subscribe(sess.ID, 32)
	defer unsub()

	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: registryLoop(&stubProvider{deltas: []string{"a"}})}); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, rt, sess.ID)

	sawRunning, sawDone := false, false
	drain := time.After(500 * time.Millisecond)
	for {
		select {
		case e := <-ch:
			if e.Kind == string(agent.KindRunning) {
				sawRunning = true
			}
			if e.Kind == string(agent.KindDone) {
				sawDone = true
			}
		case <-drain:
			if !sawRunning || !sawDone {
				t.Errorf("subscriber saw running=%v done=%v, want both true", sawRunning, sawDone)
			}
			return
		}
	}
}
