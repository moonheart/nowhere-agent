package session

import (
	"context"
	"testing"
)

// TestRecoverStrandedRuns verifies startup reconciliation marks a run left
// non-terminal by a dead process as failed, so it no longer reads as active,
// while terminal runs are left untouched.
func TestRecoverStrandedRuns(t *testing.T) {
	store := NewMemStore()
	rt := NewRuntime(store)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	// A run left "running" (as if the process died mid-run).
	stranded, _ := store.CreateRun(context.Background(), sess.ID, 1)
	_ = store.UpdateRunStatus(context.Background(), stranded.ID, RunRunning)
	// A run that finished cleanly before the restart.
	done, _ := store.CreateRun(context.Background(), sess.ID, 2)
	_ = store.UpdateRunStatus(context.Background(), done.ID, RunDone)

	n, err := rt.RecoverStrandedRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reconciled = %d want 1 (only the running run)", n)
	}

	// The stranded run must no longer read as active (which would hang attachers).
	if _, active, _ := rt.ActiveRun(context.Background(), sess.ID); active {
		t.Error("stranded run still reads as active after reconciliation")
	}

	runs, _ := store.RunsForSession(context.Background(), sess.ID)
	byID := map[string]RunStatus{}
	for _, r := range runs {
		byID[r.ID] = r.Status
	}
	if byID[stranded.ID] != RunFailed {
		t.Errorf("stranded run status = %v want failed", byID[stranded.ID])
	}
	if byID[done.ID] != RunDone {
		t.Errorf("done run status = %v want left untouched (done)", byID[done.ID])
	}
}
