package dreaming

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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
//
// When SetLock has wired a cross-instance lock (PGAdvisoryLock), the runner
// also takes it before every pass — scheduled AND manual — so a multi-instance
// deployment serializes consolidation the same way a single process does. A
// pass that cannot take it is skipped, not queued: the next tick (or a later
// button press) picks up whatever it would have.
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
	// lock, when set, is the cross-instance mutual exclusion held for the
	// whole pass (nil = in-memory single-flight only, e.g. tests).
	lock Lock

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

// SetLock wires the cross-instance lock every pass contends on. Without it the
// runner serializes only within one process; with it, a multi-instance
// deployment consolidates exactly once per watermark.
func (r *Runner) SetLock(l Lock) {
	r.lock = l
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
	if ok, err := r.claimCrossInstance(); err != nil {
		return fmt.Errorf("dreaming: cross-instance lock: %w", err)
	} else if !ok {
		return ErrBusy
	}
	go func() {
		// A panicking pass must not crash the process — and must still release
		// the single-flight + cross-instance locks, or every later trigger
		// would fail with ErrBusy and Wait() would block shutdown forever.
		// Declared after the cancel defer so it runs first (LIFO).
		defer func() {
			if p := recover(); p != nil {
				r.log.Error("dreaming: manual pass panicked", "panic", p, "stack", string(debug.Stack()))
				r.finish(userID, r.now(), Result{}, fmt.Errorf("dreaming: pass panicked: %v", p))
			}
		}()
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
	if ok, err := r.claimCrossInstance(); err != nil {
		r.log.Warn("dreaming: scheduled pass skipped, cross-instance lock failed", "err", err)
		return nil
	} else if !ok {
		r.log.Info("dreaming: scheduled pass skipped, another instance is consolidating")
		return nil
	}
	// Bound the pass like TriggerForUser does: the scheduler hands us an
	// unbounded job ctx, and a hung pass would otherwise hold both the
	// in-process single-flight lock and the PG advisory lock forever. The
	// timeout ctx derives from the scheduler's (so shutdown still cancels it).
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
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

// claimCrossInstance takes the cross-instance lock and undoes begin when it
// cannot. ok=false means the pass must not run: either another instance holds
// the lock (a skip, not an error) or taking it failed (err is set).
func (r *Runner) claimCrossInstance() (ok bool, err error) {
	if r.lock == nil {
		return true, nil
	}
	// Bound the acquisition: a hung pool must not stall a manual trigger or
	// the scheduler's tick. The pass itself is bounded by r.timeout.
	ctx, cancel := context.WithTimeout(r.base, 15*time.Second)
	defer cancel()
	got, err := r.lock.TryAcquire(ctx)
	if err != nil || !got {
		r.abort()
	}
	return got, err
}

// abort releases the in-memory single-flight claim without recording an
// outcome. It balances begin for passes that never actually ran (the
// cross-instance lock turned out to be held).
func (r *Runner) abort() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	r.owner = ""
	r.wg.Done()
}

// finish records the outcome and releases the locks — the cross-instance one
// first, so a caller waiting on Wait() can immediately start the next pass
// without racing the release.
func (r *Runner) finish(owner string, start time.Time, res Result, err error) {
	rec := RunRecord{StartedAt: start, FinishedAt: r.now(), Result: res}
	if err != nil {
		rec.Err = err.Error()
	}

	if r.lock != nil {
		if relErr := r.lock.Release(); relErr != nil {
			// Not fatal: closing the connection releases the advisory lock
			// regardless, so the pass cannot starve future ones.
			r.log.Warn("dreaming: cross-instance lock release failed", "err", relErr)
		}
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
