package session

import (
	"context"
	"testing"
)

// TestRunningRunExcludesParked pins the resume semantics behind the blank-bubble
// fix: a run parked in waiting_approval is Active (it holds the single-active
// slot for StartRun) but is NOT "running" — it has no live worker or stream, so
// clients resuming history must not treat it as in-flight. RunningRun excludes
// it; ActiveRun still counts it.
func TestRunningRunExcludesParked(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}
	run, err := rt.StartRun(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	// In-flight: both ActiveRun and RunningRun report it.
	if _, ok, _ := rt.ActiveRun(context.Background(), sess.ID); !ok {
		t.Fatal("running run should be Active")
	}
	if _, ok, _ := rt.RunningRun(context.Background(), sess.ID); !ok {
		t.Fatal("running run should be Running")
	}

	// Park it (waiting_approval). Still Active (holds the single-active slot),
	// but no longer Running — no live stream to resume.
	if _, err := rt.SuspendRun(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := rt.ActiveRun(context.Background(), sess.ID); !ok || got.Status != RunWaitingApproval {
		t.Fatalf("parked run should still be Active (waiting_approval), got %+v ok=%v", got, ok)
	}
	if _, ok, _ := rt.RunningRun(context.Background(), sess.ID); ok {
		t.Error("parked run must NOT be Running (no live stream to resume)")
	}

	// A resumed run becomes Running again.
	if _, err := rt.ResumeRun(context.Background(), sess.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := rt.RunningRun(context.Background(), sess.ID); !ok {
		t.Error("resumed run should be Running again")
	}
}
