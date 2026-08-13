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
	// Run executes the job. It should respect ctx cancellation. Runs never
	// overlap for the same job; different jobs run concurrently.
	Run func(ctx context.Context) error
}

// Scheduler runs jobs on their intervals with catch-up semantics. Intervals
// are mutable via SetInterval, so an operator can retune a job's cadence
// without a restart; due checks read the CURRENT interval each tick. Each due
// job runs in its OWN goroutine (an in-flight mark stops the same job from
// overlapping), so a slow job — dreaming, say — never blocks the scheduler
// loop, sibling jobs, SetInterval, or LastRun: the mutex only guards
// bookkeeping.
type Scheduler struct {
	log    *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	jobs     map[string]Job
	lastRun  map[string]time.Time
	inflight map[string]bool
	wg       sync.WaitGroup
}

// New creates a Scheduler. Times are UTC.
func New(log *slog.Logger, jobs ...Job) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	s := &Scheduler{
		jobs:     map[string]Job{},
		log:      log,
		now:      func() time.Time { return time.Now().UTC() },
		lastRun:  map[string]time.Time{},
		inflight: map[string]bool{},
	}
	for _, j := range jobs {
		s.jobs[j.Name] = j
	}
	return s
}

// SetInterval retunes a job's cadence live. A non-positive interval is
// ignored. The next due check uses the new interval.
func (s *Scheduler) SetInterval(name string, d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	j, ok := s.jobs[name]
	if ok {
		j.Interval = d
		s.jobs[name] = j
	}
	s.mu.Unlock()
}

// tick is injectable for tests.
var tickInterval = time.Second

// Start runs the scheduler until ctx is cancelled. On startup it performs
// catch-up: any job that has never run (or whose interval already elapsed)
// runs immediately. When ctx is cancelled it waits for in-flight runs to wind
// down (they receive ctx) before returning.
func (s *Scheduler) Start(ctx context.Context) {
	s.catchUp(ctx)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// No dispatches can start after the loop exits; wait for the runs
			// that are already in flight.
			s.wg.Wait()
			return
		case now := <-ticker.C:
			s.runDue(ctx, now)
		}
	}
}

// catchUp dispatches jobs that are overdue at startup (never ran, or interval
// passed). The dispatch is asynchronous; the due stamping is synchronous.
func (s *Scheduler) catchUp(ctx context.Context) {
	s.mu.Lock()
	due := s.markDueLocked(s.now(), true)
	s.wg.Add(len(due))
	s.mu.Unlock()
	s.dispatch(ctx, due)
}

// runDue dispatches any jobs whose interval has elapsed since their last run.
// The CURRENT interval (possibly retuned by SetInterval) applies, so a retune
// takes effect at the next tick.
func (s *Scheduler) runDue(ctx context.Context, now time.Time) {
	s.mu.Lock()
	due := s.markDueLocked(now, false)
	s.wg.Add(len(due))
	s.mu.Unlock()
	s.dispatch(ctx, due)
}

// markDueLocked selects jobs that are due — interval elapsed since lastRun (or
// never run, when firstRunOK) AND not already in flight — stamps lastRun and
// marks them in flight. Caller holds mu. The in-flight mark is what keeps a
// slow job from overlapping itself across ticks.
func (s *Scheduler) markDueLocked(now time.Time, firstRunOK bool) []Job {
	var due []Job
	for name, j := range s.jobs {
		last, ok := s.lastRun[name]
		if !ok {
			if !firstRunOK {
				continue // catchUp already handled first run
			}
		} else if now.Sub(last) < j.Interval {
			continue
		}
		if s.inflight[name] {
			continue // previous run still going — never overlap the same job
		}
		s.lastRun[name] = now
		s.inflight[name] = true
		due = append(due, j)
	}
	return due
}

// dispatch starts each due job in its own goroutine.
func (s *Scheduler) dispatch(ctx context.Context, due []Job) {
	for _, j := range due {
		go s.run(ctx, j)
	}
}

// run executes one job and clears its in-flight mark on completion. The lock
// is only touched for the bookkeeping; the job itself runs outside it.
func (s *Scheduler) run(ctx context.Context, j Job) {
	defer s.wg.Done()
	if err := j.Run(ctx); err != nil {
		s.log.Error("scheduled job failed", "job", j.Name, "error", err)
	} else {
		s.log.Debug("scheduled job ran", "job", j.Name)
	}
	s.mu.Lock()
	delete(s.inflight, j.Name)
	s.mu.Unlock()
}

// LastRun reports when a job last ran (for tests/inspection).
func (s *Scheduler) LastRun(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastRun[name]
	return t, ok
}
