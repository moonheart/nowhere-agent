package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatchUpRunsNeverRunJobs(t *testing.T) {
	var ran atomic.Int32
	s := New(nil, Job{Name: "j", Interval: time.Hour, Run: func(context.Context) error {
		ran.Add(1)
		return nil
	}})
	s.catchUp(context.Background())
	if ran.Load() != 1 {
		t.Errorf("catchUp ran %d times want 1", ran.Load())
	}
}

func TestCatchUpRecordsLastRun(t *testing.T) {
	s := New(nil, Job{Name: "j", Interval: time.Hour, Run: func(context.Context) error { return nil }})
	if _, ok := s.LastRun("j"); ok {
		t.Error("should not have run yet")
	}
	s.catchUp(context.Background())
	if _, ok := s.LastRun("j"); !ok {
		t.Error("lastRun should be recorded after catchUp")
	}
}

func TestRunDueOnlyAfterInterval(t *testing.T) {
	var ran atomic.Int32
	s := New(nil, Job{Name: "j", Interval: time.Minute, Run: func(context.Context) error {
		ran.Add(1)
		return nil
	}})
	// Simulate a recent run.
	s.lastRun["j"] = s.now()

	// Not due yet.
	s.runDue(context.Background(), s.now().Add(30*time.Second))
	if ran.Load() != 0 {
		t.Error("job ran before interval elapsed")
	}

	// Due after interval.
	s.runDue(context.Background(), s.now().Add(2*time.Minute))
	if ran.Load() != 1 {
		t.Errorf("job ran %d times want 1 after interval", ran.Load())
	}
}

func TestJobErrorDoesNotCrashScheduler(t *testing.T) {
	s := New(nil, Job{Name: "bad", Interval: time.Second, Run: func(context.Context) error {
		return errors.New("boom")
	}})
	// Should not panic.
	s.catchUp(context.Background())
	if _, ok := s.LastRun("bad"); !ok {
		t.Error("lastRun should be recorded even on error")
	}
}

func TestUTCNow(t *testing.T) {
	s := New(nil)
	if s.now().Location() != time.UTC {
		t.Errorf("scheduler time not UTC: %v", s.now().Location())
	}
}
