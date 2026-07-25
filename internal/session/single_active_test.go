package session

import (
	"context"
	"testing"
)

// TestStartRunRejectsCrossInstanceActiveRun verifies the single-active-run lock
// is enforced via the durable store, not just the per-process in-memory map: a
// second runtime sharing the same store must reject a run for a session that
// already has one active.
func TestStartRunRejectsCrossInstanceActiveRun(t *testing.T) {
	store := NewMemStore()
	instanceA := NewRuntime(store)
	instanceB := NewRuntime(store) // shares the durable store, has its own in-memory lock

	sess, err := instanceA.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := instanceA.StartRun(context.Background(), sess.ID); err != nil {
		t.Fatalf("instance A StartRun: %v", err)
	}

	// B's in-memory map is empty, but the run is active in the shared store, so
	// B must still reject it (before the fix, B's in-memory-only check passed and
	// a second concurrent run started).
	if _, err := instanceB.StartRun(context.Background(), sess.ID); err != ErrRunActive {
		t.Errorf("instance B StartRun = %v, want ErrRunActive", err)
	}
}

// TestStartRunAllowsNextRunAfterSettle verifies a session can start a new run
// once the previous one has settled (the store check must not over-block).
func TestStartRunAllowsNextRunAfterSettle(t *testing.T) {
	store := NewMemStore()
	rt := NewRuntime(store)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	run, err := rt.StartRun(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.CompleteRun(context.Background(), sess.ID, RunDone); err != nil {
		t.Fatal(err)
	}
	_ = run
	if _, err := rt.StartRun(context.Background(), sess.ID); err != nil {
		t.Errorf("second StartRun after settle = %v, want success", err)
	}
}
