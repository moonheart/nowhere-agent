// Package scheduler implements the unified scheduler (design D15): one
// component drives all periodic/delayed work — dreaming runs, sandbox deferred
// destroy, quota rollover — with UTC storage and catch-up after restarts.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Job is a named periodic task.
type Job struct {
	Name     string
	Interval time.Duration
	// Run executes the job. It should respect ctx cancellation.
	Run func(ctx context.Context) error
}

// Scheduler runs jobs on their intervals with catch-up semantics.
type Scheduler struct {
	jobs   []Job
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	lastRun map[string]time.Time
}

// New creates a Scheduler. Times are UTC.
func New(log *slog.Logger, jobs ...Job) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{jobs: jobs, log: log, now: func() time.Time { return time.Now().UTC() }, lastRun: map[string]time.Time{}}
}

// tick is injectable for tests.
var tickInterval = time.Second

// Start runs the scheduler until ctx is cancelled. On startup it performs
// catch-up: any job that has never run (or whose interval already elapsed)
// runs immediately.
func (s *Scheduler) Start(ctx context.Context) {
	s.catchUp(ctx)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.runDue(ctx, now)
		}
	}
}

// catchUp runs jobs that are overdue at startup (never ran, or interval passed).
func (s *Scheduler) catchUp(ctx context.Context) {
	now := s.now()
	for _, j := range s.jobs {
		last, ok := s.lastRun[j.Name]
		if !ok || now.Sub(last) >= j.Interval {
			s.runJob(ctx, j)
		}
	}
}

// runDue runs any jobs whose interval has elapsed since their last run.
func (s *Scheduler) runDue(ctx context.Context, now time.Time) {
	for _, j := range s.jobs {
		s.mu.Lock()
		last, ok := s.lastRun[j.Name]
		s.mu.Unlock()
		if !ok {
			continue // catchUp already handled first run
		}
		if now.Sub(last) >= j.Interval {
			s.runJob(ctx, j)
		}
	}
}

// runJob executes one job and records its completion time.
func (s *Scheduler) runJob(ctx context.Context, j Job) {
	s.mu.Lock()
	s.lastRun[j.Name] = s.now()
	s.mu.Unlock()

	if err := j.Run(ctx); err != nil {
		s.log.Error("scheduled job failed", "job", j.Name, "error", err)
		return
	}
	s.log.Debug("scheduled job ran", "job", j.Name)
}

// LastRun reports when a job last ran (for tests/inspection).
func (s *Scheduler) LastRun(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastRun[name]
	return t, ok
}
