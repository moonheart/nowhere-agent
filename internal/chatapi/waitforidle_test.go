package chatapi

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/session"
)

// TestWaitForIdle covers the resume-vs-settle guard: waitForIdle must report
// idle immediately when nothing is running, time out while a run holds the
// single-active-run lock, and return true (having waited) once that run
// completes. This is what lets a verdict POST that races the parking run's
// settle succeed instead of 409-ing on a run that is already on its way out.
func TestWaitForIdle(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore()).WithBus(session.NewMemBus())
	h := NewHandler(func(context.Context, string) *agent.Loop { return nil }, "sys").WithRuntime(rt)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	// Idle immediately when no run is active.
	if !h.waitForIdle(context.Background(), sess.ID, time.Second) {
		t.Fatal("should report idle with no active run")
	}

	// An active run holds the lock: waitForIdle times out.
	if _, err := rt.StartRun(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if h.waitForIdle(context.Background(), sess.ID, 80*time.Millisecond) {
		t.Fatal("should time out while a run is active")
	}

	// The run settles mid-wait: waitForIdle returns true, having waited for it.
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = rt.CompleteRun(context.Background(), sess.ID, session.RunDone)
	}()
	start := time.Now()
	if !h.waitForIdle(context.Background(), sess.ID, 2*time.Second) {
		t.Fatal("should go idle once the active run completes")
	}
	if waited := time.Since(start); waited < 30*time.Millisecond {
		t.Errorf("returned in %v; expected to wait ~40ms for the run to settle", waited)
	}
}

// TestWaitForIdleHonoursContext ensures a cancelled request unblocks the wait
// (the resume client disconnected) rather than spinning to the timeout.
func TestWaitForIdleHonoursContext(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore()).WithBus(session.NewMemBus())
	h := NewHandler(func(context.Context, string) *agent.Loop { return nil }, "sys").WithRuntime(rt)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartRun(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if h.waitForIdle(ctx, sess.ID, 5*time.Second) {
		t.Fatal("should not report idle while the run is still active")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("returned in %v; expected to unblock on ctx cancel (~30ms), not run to timeout", waited)
	}
}
