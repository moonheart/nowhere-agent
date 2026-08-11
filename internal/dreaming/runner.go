package dreaming

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrBusy is returned when a consolidation pass is already in flight.
var ErrBusy = errors.New("a dreaming pass is already running")

// RunRecord is the outcome of one completed pass.
type RunRecord struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Result     Result
	// Err is the failure message, empty on success. It is a string rather than
	// an error because it is read long after the pass, by a status endpoint.
	Err string
}

// RunState is what a caller sees when asking about consolidation.
type RunState struct {
	// Running reports whether ANY pass is in flight — including the scheduled
	// one, which consolidates this caller's sessions too.
	Running bool
	// Mine narrows that to a pass this caller triggered.
	Mine bool
	// Last is the caller's own most recent triggered pass, nil if they have
	// never triggered one.
	Last *RunRecord
}

// Runner serializes dreaming passes and runs manual ones in the background.
//
// The lock is process-wide and covers the scheduled pass as well as manual
// ones, which is stronger than it first appears necessary. It has to be: two
// concurrent passes both read a session's dreamed_seq before either advances
// it, so both consolidate the same messages and neither can see the other's
// writes — the store gains a duplicate set of memories from one episode. The
// watermark makes a pass idempotent against ITSELF, not against a second pass
// racing it.
type Runner struct {
	worker  *Worker
	base    context.Context
	timeout time.Duration
	log     *slog.Logger
	now     func() time.Time
	// knobSync, when set, is invoked before every pass (scheduled AND manual)
	// so runtime-settable knobs (budget, caps, purge window) apply to whatever
	// path starts the pass.
	knobSync func()

	mu      sync.Mutex
	wg      sync.WaitGroup
	running bool
	owner   string // "" while the scheduled pass holds it
	last    map[string]RunRecord
}

// NewRunner wraps a worker. base is the process's root context: a manual pass
// outlives the HTTP request that asked for it, so it must not inherit that
// request's cancellation, but it must still stop when the server does.
func NewRunner(w *Worker, base context.Context) *Runner {
	return &Runner{
		worker:  w,
		base:    base,
		timeout: 30 * time.Minute,
		log:     slog.Default(),
		now:     time.Now,
		last:    map[string]RunRecord{},
	}
}

// SetLogger overrides the runner's logger.
func (r *Runner) SetLogger(l *slog.Logger) {
	if l != nil {
		r.log = l
	}
}

// SetTimeout bounds a single pass. A pass that hangs must not hold the
// single-flight lock forever, or manual consolidation is dead until restart.
func (r *Runner) SetTimeout(d time.Duration) {
	if d > 0 {
		r.timeout = d
	}
}

// SetClock overrides the runner's clock (tests).
func (r *Runner) SetClock(now func() time.Time) {
	if now != nil {
		r.now = now
	}
}

// SetKnobSync wires a function applied before every pass, scheduled or
// manual. It lets the server retune the worker's budget/caps/purge from the
// runtime settings right before each run.
func (r *Runner) SetKnobSync(f func()) {
	r.knobSync = f
}

// syncKnobs runs the knob-sync hook (nil-safe).
func (r *Runner) syncKnobs() {
	if r.knobSync != nil {
		r.knobSync()
	}
}

// TriggerForUser starts a pass over one account's sessions in the background,
// returning immediately. It returns ErrBusy when a pass is already running —
// the caller is told to wait rather than being queued, because a queued
// duplicate would consolidate an empty tail and spend tokens proving it.
func (r *Runner) TriggerForUser(userID string) error {
	if userID == "" {
		return errors.New("dreaming: no user to consolidate for")
	}
	if !r.begin(userID) {
		return ErrBusy
	}
	go func() {
		ctx, cancel := context.WithTimeout(r.base, r.timeout)
		defer cancel()
		start := r.now()
		r.syncKnobs()
		res, err := r.worker.RunForUser(ctx, userID)
		r.finish(userID, start, res, err)
	}()
	return nil
}

// RunScheduled is the scheduler's entry point. It runs synchronously (the
// scheduler already owns a goroutine) and SKIPS rather than fails when a manual
// pass holds the lock: a missed tick is not an error, and the next tick will
// pick up whatever this one would have.
func (r *Runner) RunScheduled(ctx context.Context) error {
	if !r.begin("") {
		r.log.Info("dreaming: scheduled pass skipped, another pass is in flight")
		return nil
	}
	start := r.now()
	r.syncKnobs()
	res, err := r.worker.Run(ctx)
	r.finish("", start, res, err)
	return err
}

// Status reports what a caller can see: whether a pass is in flight, whether it
// is theirs, and how their last triggered pass went.
func (r *Runner) Status(userID string) RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := RunState{Running: r.running, Mine: r.running && r.owner == userID && userID != ""}
	if rec, ok := r.last[userID]; ok {
		st.Last = &rec
	}
	return st
}

// Wait blocks until no pass is in flight. It exists for graceful shutdown and
// for tests, which would otherwise have to poll a background goroutine.
func (r *Runner) Wait() { r.wg.Wait() }

// begin claims the single-flight lock, reporting whether it succeeded.
func (r *Runner) begin(owner string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	r.owner = owner
	r.wg.Add(1)
	return true
}

// finish records the outcome and releases the lock.
func (r *Runner) finish(owner string, start time.Time, res Result, err error) {
	rec := RunRecord{StartedAt: start, FinishedAt: r.now(), Result: res}
	if err != nil {
		rec.Err = err.Error()
	}

	r.mu.Lock()
	r.running = false
	r.owner = ""
	r.last[owner] = rec
	r.mu.Unlock()
	r.wg.Done()

	if err != nil {
		r.log.Warn("dreaming: pass failed", "owner", owner, "err", err,
			"tokens", res.TokensUsed, "episodes", res.EpisodesProcessed)
		return
	}
	r.log.Info("dreaming: pass complete", "owner", owner,
		"episodes", res.EpisodesProcessed, "added", res.MemoriesWritten,
		"revised", res.MemoriesRevised, "retired", res.MemoriesRetired,
		"purged", res.MemoriesPurged, "tokens", res.TokensUsed,
		"budget_exhausted", res.BudgetExhausted,
		"duration", rec.FinishedAt.Sub(start).Round(time.Millisecond))
}
