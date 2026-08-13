package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// mustNew builds a scheduler for tests, failing on an invalid job set.
func mustNew(t *testing.T, jobs ...Job) *Scheduler {
	t.Helper()
	s, err := New(nil, jobs...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// waitFor polls cond until it holds or a deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestCatchUpRunsNeverRunJobs(t *testing.T) {
	var ran atomic.Int32
	s := mustNew(t, Job{Name: "j", Interval: time.Hour, Run: func(context.Context) error {
		ran.Add(1)
		return nil
	}})
	s.catchUp(context.Background())
	waitFor(t, func() bool { return ran.Load() == 1 })

	// A second catch-up must NOT re-run: lastRun is stamped at dispatch.
	s.catchUp(context.Background())
	waitFor(t, func() bool { return ran.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if ran.Load() != 1 {
		t.Errorf("catchUp ran %d times want 1", ran.Load())
	}
}

func TestCatchUpRecordsLastRun(t *testing.T) {
	s := mustNew(t, Job{Name: "j", Interval: time.Hour, Run: func(context.Context) error { return nil }})
	if _, ok := s.LastRun("j"); ok {
		t.Error("should not have run yet")
	}
	s.catchUp(context.Background())
	waitFor(t, func() bool {
		_, ok := s.LastRun("j")
		return ok
	})
}

func TestRunDueOnlyAfterInterval(t *testing.T) {
	var ran atomic.Int32
	s := mustNew(t, Job{Name: "j", Interval: time.Minute, Run: func(context.Context) error {
		ran.Add(1)
		return nil
	}})
	// Simulate a recent run.
	s.lastRun["j"] = s.now()

	// Not due yet: no dispatch happens (due selection is synchronous), but give
	// a wrongly-dispatched run a grace window to surface.
	s.runDue(context.Background(), s.now().Add(30*time.Second))
	time.Sleep(50 * time.Millisecond)
	if ran.Load() != 0 {
		t.Error("job ran before interval elapsed")
	}

	// Due after interval.
	s.runDue(context.Background(), s.now().Add(2*time.Minute))
	waitFor(t, func() bool { return ran.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if ran.Load() != 1 {
		t.Errorf("job ran %d times want 1 after interval", ran.Load())
	}
}

func TestJobErrorDoesNotCrashScheduler(t *testing.T) {
	var ran atomic.Int32
	s := mustNew(t, Job{Name: "bad", Interval: time.Second, Run: func(context.Context) error {
		ran.Add(1)
		return errors.New("boom")
	}})
	// Should not panic: the error is contained and the scheduler survives.
	// lastRun is stamped synchronously at dispatch, so wait on the RUN itself
	// (mirroring TestCatchUpRunsNeverRunJobs) or a dispatch that never executes
	// would pass.
	s.catchUp(context.Background())
	waitFor(t, func() bool { return ran.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if ran.Load() != 1 {
		t.Errorf("job ran %d times want 1", ran.Load())
	}
}

// A slow job runs in its own goroutine: while it is blocked, sibling jobs must
// still fire and LastRun must stay responsive (the mutex never wraps Run).
func TestSlowJobDoesNotBlockSiblings(t *testing.T) {
	release := make(chan struct{})
	slow := Job{Name: "slow", Interval: time.Minute, Run: func(context.Context) error {
		<-release // block until the test releases it
		return nil
	}}
	var fast atomic.Int32
	s := mustNew(t, slow, Job{Name: "fast", Interval: time.Minute, Run: func(context.Context) error {
		fast.Add(1)
		return nil
	}})
	s.lastRun["slow"] = s.now().Add(-time.Hour)
	s.lastRun["fast"] = s.now().Add(-time.Hour)

	s.runDue(context.Background(), s.now())
	waitFor(t, func() bool { return fast.Load() == 1 })
	if _, ok := s.LastRun("slow"); !ok {
		t.Error("slow job's lastRun should be stamped despite it still running")
	}
	close(release)
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.inflight["slow"]
	})
}

// The in-flight mark prevents the same job from overlapping itself: ticks
// while a run is still going must not start a second run.
func TestJobDoesNotOverlapItself(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	s := mustNew(t, Job{Name: "j", Interval: time.Millisecond, Run: func(context.Context) error {
		runs.Add(1)
		entered <- struct{}{}
		<-release
		return nil
	}})
	s.lastRun["j"] = s.now().Add(-time.Hour) // due immediately

	s.runDue(context.Background(), s.now())
	<-entered // first run is in flight
	// Two more ticks while it is still blocked.
	s.runDue(context.Background(), s.now().Add(time.Second))
	s.runDue(context.Background(), s.now().Add(2*time.Second))
	select {
	case <-entered:
		t.Fatal("job overlapped itself")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.inflight["j"]
	})
	if runs.Load() != 1 {
		t.Errorf("runs = %d, want 1", runs.Load())
	}
}

// Start performs catch-up on launch and keeps ticking after the interval.
func TestStartCatchUpAndTick(t *testing.T) {
	var runs atomic.Int32
	s := mustNew(t, Job{Name: "j", Interval: time.Hour, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx)
	waitFor(t, func() bool { return runs.Load() == 1 }) // startup catch-up

	s.SetInterval("j", time.Millisecond) // retune live: next tick fires again
	waitFor(t, func() bool { return runs.Load() >= 2 })
}

func TestUTCNow(t *testing.T) {
	s := mustNew(t)
	if s.now().Location() != time.UTC {
		t.Errorf("scheduler time not UTC: %v", s.now().Location())
	}
}

func TestNewRejectsNonPositiveInterval(t *testing.T) {
	if _, err := New(nil, Job{Name: "bad", Interval: 0, Run: func(context.Context) error { return nil }}); err == nil {
		t.Error("zero interval must be refused")
	}
	if _, err := New(nil, Job{Name: "bad", Interval: -time.Second, Run: func(context.Context) error { return nil }}); err == nil {
		t.Error("negative interval must be refused")
	}
	if _, err := New(nil, Job{Name: "ok", Interval: time.Second, Run: func(context.Context) error { return nil }}); err != nil {
		t.Errorf("valid job refused: %v", err)
	}
}

// A job that ignores ctx cancellation must not hold shutdown forever: Start
// returns once the bounded wait elapses.
func TestStartShutdownTimesOutOnHungJob(t *testing.T) {
	old := stopTimeout
	stopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stopTimeout = old })

	blocked := make(chan struct{})
	s := mustNew(t, Job{Name: "hung", Interval: time.Hour, Run: func(context.Context) error {
		<-blocked // never returns, ignores ctx
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(done)
	}()
	// Wait for the job to start, then cancel and expect a prompt return.
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inflight["hung"]
	})
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return within the stop timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("shutdown took %v, want bounded by stopTimeout", elapsed)
	}
	close(blocked)
}
