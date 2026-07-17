package session

import (
	"context"
	"testing"
	"time"
)

func setup(t *testing.T) (*Runtime, *MemStore, Session) {
	t.Helper()
	store := NewMemStore()
	rt := NewRuntime(store)
	sess, err := store.CreateSession(context.Background(), "user1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return rt, store, sess
}

func TestStartRunSetsRunning(t *testing.T) {
	rt, _, sess := setup(t)
	run, err := rt.StartRun(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunRunning {
		t.Errorf("status = %q want running", run.Status)
	}
	if run.Seq != 1 {
		t.Errorf("seq = %d want 1", run.Seq)
	}
}

func TestSingleActiveRunEnforced(t *testing.T) {
	rt, _, sess := setup(t)
	ctx := context.Background()
	if _, err := rt.StartRun(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	// Second start while active must fail (multi-writer prevention, D13).
	if _, err := rt.StartRun(ctx, sess.ID); err != ErrRunActive {
		t.Errorf("expected ErrRunActive, got %v", err)
	}
}

func TestCompleteRunReleasesLock(t *testing.T) {
	rt, _, sess := setup(t)
	ctx := context.Background()
	if _, err := rt.StartRun(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := rt.CompleteRun(ctx, sess.ID, RunDone); err != nil {
		t.Fatal(err)
	}
	// Now a new run can start.
	run, err := rt.StartRun(ctx, sess.ID)
	if err != nil {
		t.Fatalf("expected new run to start, got %v", err)
	}
	if run.Seq != 2 {
		t.Errorf("seq = %d want 2", run.Seq)
	}
}

func TestCompleteRunRequiresTerminal(t *testing.T) {
	rt, _, sess := setup(t)
	ctx := context.Background()
	rt.StartRun(ctx, sess.ID)
	if err := rt.CompleteRun(ctx, sess.ID, RunRunning); err == nil {
		t.Error("expected error for non-terminal completion")
	}
}

func TestStartRunOnEndedSession(t *testing.T) {
	rt, store, sess := setup(t)
	store.EndSession(context.Background(), sess.ID)
	if _, err := rt.StartRun(context.Background(), sess.ID); err != ErrSessionEnded {
		t.Errorf("expected ErrSessionEnded, got %v", err)
	}
}

func TestAppendEventAssignsOffsetsAndFansOut(t *testing.T) {
	rt, _, sess := setup(t)
	ctx := context.Background()
	run, _ := rt.StartRun(ctx, sess.ID)

	ch, unsub := rt.Subscribe(sess.ID, 10)
	defer unsub()

	e1 := Event{RunID: run.ID, SessionID: sess.ID, Kind: "message", Payload: []byte(`"hi"`)}
	e2 := Event{RunID: run.ID, SessionID: sess.ID, Kind: "message", Payload: []byte(`"there"`)}
	if err := rt.AppendEvent(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if err := rt.AppendEvent(ctx, e2); err != nil {
		t.Fatal(err)
	}

	// Offsets assigned monotonically.
	got1 := <-ch
	got2 := <-ch
	if got1.Offset != 1 || got2.Offset != 2 {
		t.Errorf("offsets = %d,%d want 1,2", got1.Offset, got2.Offset)
	}
}

func TestReplayReturnsEventsAfterOffset(t *testing.T) {
	rt, _, sess := setup(t)
	ctx := context.Background()
	run, _ := rt.StartRun(ctx, sess.ID)
	for i := 0; i < 3; i++ {
		rt.AppendEvent(ctx, Event{RunID: run.ID, SessionID: sess.ID, Kind: "message", Payload: []byte(`"x"`)})
	}
	events, err := rt.Replay(ctx, run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("replay returned %d events want 2", len(events))
	}
	if events[0].Offset != 2 || events[1].Offset != 3 {
		t.Errorf("replayed offsets = %d,%d", events[0].Offset, events[1].Offset)
	}
}

func TestActiveRunLookup(t *testing.T) {
	rt, _, sess := setup(t)
	ctx := context.Background()
	if _, ok, _ := rt.ActiveRun(ctx, sess.ID); ok {
		t.Error("no run expected yet")
	}
	rt.StartRun(ctx, sess.ID)
	if _, ok, _ := rt.ActiveRun(ctx, sess.ID); !ok {
		t.Error("expected active run")
	}
}

func TestListIdleSessions(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	sess, _ := store.CreateSession(ctx, "u", "t")
	// Force the session to look old.
	store.mu.Lock()
	s := store.sessions[sess.ID]
	s.UpdatedAt = time.Now().Add(-time.Hour)
	store.mu.Unlock()

	idle, err := store.ListIdleSessions(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(idle) != 1 {
		t.Errorf("expected 1 idle session, got %d", len(idle))
	}
}
