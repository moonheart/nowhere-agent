package session

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrRunActive is returned when starting a run while one is already active.
	ErrRunActive = errors.New("a run is already active in this session")
	// ErrNoActiveRun is returned when completing/cancelling with no active run.
	ErrNoActiveRun = errors.New("no active run in this session")
	// ErrSessionEnded is returned when acting on an ended session.
	ErrSessionEnded = errors.New("session has ended")
)

// Store persists sessions, runs, and events. Implemented over Postgres.
type Store interface {
	CreateSession(ctx context.Context, userID, title string) (Session, error)
	GetSession(ctx context.Context, id string) (Session, error)
	EndSession(ctx context.Context, id string) error
	// ListSessionsByUser returns a user's sessions, most-recently-active first.
	ListSessionsByUser(ctx context.Context, userID string) ([]Session, error)
	// DeleteSessionForUser soft-deletes (ends) a session owned by userID,
	// returning false if no such session exists for that user.
	DeleteSessionForUser(ctx context.Context, id, userID string) (bool, error)
	// ListIdleSessions returns active sessions with no event activity since
	// the given time (candidates for idle-end by the scheduler).
	ListIdleSessions(ctx context.Context, idleSinceEventBefore time.Time) ([]Session, error)

	CreateRun(ctx context.Context, sessionID string, seq int) (Run, error)
	UpdateRunStatus(ctx context.Context, runID string, status RunStatus) error
	// ActiveRun returns the active run in a session, or false.
	ActiveRun(ctx context.Context, sessionID string) (Run, bool, error)
	// NextRunSeq returns the next sequence number for a session's run.
	NextRunSeq(ctx context.Context, sessionID string) (int, error)
	// RunsForSession returns all runs in a session, for history replay.
	RunsForSession(ctx context.Context, sessionID string) ([]Run, error)

	AppendEvent(ctx context.Context, e Event) error
	// EventsAfter returns events for a run with offset > after, ordered.
	EventsAfter(ctx context.Context, runID string, after int) ([]Event, error)
}

// Runtime coordinates run lifecycle, the single-active-run lock, and event
// fan-out to attached clients. Transport (WS/SSE) subscribes; Runtime owns state.
// Fan-out goes through an EventBus (in-memory single-instance by default) so a
// Redis-backed bus can fan out across instances without this layer changing.
type Runtime struct {
	store Store
	bus   EventBus

	mu   sync.Mutex
	runs map[string]*runState // sessionID -> active run state
}

type runState struct {
	run    Run
	offset int
	// cancel, when registered, interrupts the run's in-flight work (the agent
	// loop + tool dispatch). Set via RegisterCancel; cleared on CompleteRun.
	cancel context.CancelFunc
}

// NewRuntime creates a Runtime over a Store, fanning events out over an
// in-memory bus. Use WithBus to substitute a different EventBus (e.g. Redis).
func NewRuntime(store Store) *Runtime {
	return &Runtime{
		store: store,
		bus:   NewMemBus(),
		runs:  map[string]*runState{},
	}
}

// WithBus sets the EventBus used for fan-out and returns the Runtime.
func (rt *Runtime) WithBus(bus EventBus) *Runtime {
	rt.bus = bus
	return rt
}

// Bus returns the Runtime's EventBus, for transports that subscribe directly.
func (rt *Runtime) Bus() EventBus { return rt.bus }

// StartRun begins a new run, enforcing single-active-run. Returns ErrRunActive
// if a run is already in progress; ErrSessionEnded if the session has ended.
func (rt *Runtime) StartRun(ctx context.Context, sessionID string) (Run, error) {
	sess, err := rt.store.GetSession(ctx, sessionID)
	if err != nil {
		return Run{}, err
	}
	if sess.Status == SessionEnded {
		return Run{}, ErrSessionEnded
	}

	rt.mu.Lock()
	if _, active := rt.runs[sessionID]; active {
		rt.mu.Unlock()
		return Run{}, ErrRunActive
	}
	rt.mu.Unlock()

	seq, err := rt.store.NextRunSeq(ctx, sessionID)
	if err != nil {
		return Run{}, err
	}
	run, err := rt.store.CreateRun(ctx, sessionID, seq)
	if err != nil {
		return Run{}, err
	}
	if err := rt.store.UpdateRunStatus(ctx, run.ID, RunRunning); err != nil {
		return Run{}, err
	}
	run.Status = RunRunning

	rt.mu.Lock()
	rt.runs[sessionID] = &runState{run: run}
	rt.mu.Unlock()
	return run, nil
}

// AppendEvent persists an event (flushing the iteration to the DB) and fans it
// out to subscribers. This is the single write path for run output.
func (rt *Runtime) AppendEvent(ctx context.Context, e Event) error {
	rt.mu.Lock()
	rs, ok := rt.runs[e.SessionID]
	if ok {
		rs.offset++
		e.Offset = rs.offset
	}
	rt.mu.Unlock()

	// The run may already have settled (e.g. CancelRun settled it while the loop
	// was unwinding its final KindCancelled event). Continue the offset from the
	// durable log rather than restarting at 0, so the event is still stored (and
	// replayed) at a sensible position instead of colliding at offset 0.
	if !ok {
		if events, err := rt.store.EventsAfter(ctx, e.RunID, 0); err == nil && len(events) > 0 {
			e.Offset = events[len(events)-1].Offset + 1
		}
	}

	// Persist first (the durable log is the source of truth), then fan out live.
	if err := rt.store.AppendEvent(ctx, e); err != nil {
		return err
	}
	rt.bus.Publish(e.SessionID, e)
	return nil
}

// CompleteRun marks the active run done/failed/cancelled and releases the lock.
func (rt *Runtime) CompleteRun(ctx context.Context, sessionID string, status RunStatus) error {
	if !status.Terminal() {
		return errors.New("status must be terminal")
	}
	rt.mu.Lock()
	rs, ok := rt.runs[sessionID]
	if !ok {
		rt.mu.Unlock()
		return ErrNoActiveRun
	}
	delete(rt.runs, sessionID)
	rt.mu.Unlock()
	return rt.store.UpdateRunStatus(ctx, rs.run.ID, status)
}

// ActiveRun returns the active run for a session, or false.
func (rt *Runtime) ActiveRun(ctx context.Context, sessionID string) (Run, bool, error) {
	rt.mu.Lock()
	rs, ok := rt.runs[sessionID]
	rt.mu.Unlock()
	if ok {
		return rs.run, true, nil
	}
	return rt.store.ActiveRun(ctx, sessionID)
}

// GetSession fetches a session by id (pass-through to the store).
func (rt *Runtime) GetSession(ctx context.Context, id string) (Session, error) {
	return rt.store.GetSession(ctx, id)
}

// CreateSession creates a session for a user (pass-through to the store).
func (rt *Runtime) CreateSession(ctx context.Context, userID, title string) (Session, error) {
	return rt.store.CreateSession(ctx, userID, title)
}

// Subscribe returns a channel receiving new events for a session, plus an
// unsubscribe func. Combined with Replay it implements reconnect-and-replay.
// It delegates to the Runtime's EventBus.
func (rt *Runtime) Subscribe(sessionID string, buffer int) (<-chan Event, func()) {
	return rt.bus.Subscribe(sessionID, buffer)
}

// Replay returns persisted events for a run after the given offset, for a
// reconnecting client to catch up.
func (rt *Runtime) Replay(ctx context.Context, runID string, after int) ([]Event, error) {
	return rt.store.EventsAfter(ctx, runID, after)
}

// RunsForSession returns every run in a session (any state), for history replay.
func (rt *Runtime) RunsForSession(ctx context.Context, sessionID string) ([]Run, error) {
	return rt.store.RunsForSession(ctx, sessionID)
}

// ListSessionsByUser returns a user's sessions for the conversation list.
func (rt *Runtime) ListSessionsByUser(ctx context.Context, userID string) ([]Session, error) {
	return rt.store.ListSessionsByUser(ctx, userID)
}

// DeleteSessionForUser ends a session the user owns; false if not theirs.
func (rt *Runtime) DeleteSessionForUser(ctx context.Context, id, userID string) (bool, error) {
	return rt.store.DeleteSessionForUser(ctx, id, userID)
}

// RegisterCancel attaches the cancel func that interrupts the active run's
// work to the session's run state, so CancelRun can stop it. The handler calls
// this right after deriving a cancellable context for the run. No-op if no run
// is active.
func (rt *Runtime) RegisterCancel(sessionID string, cancel context.CancelFunc) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rs, ok := rt.runs[sessionID]; ok {
		rs.cancel = cancel
	}
}

// CancelRun stops the session's active run: it invokes the registered cancel
// func (interrupting the loop + tools) and marks the run RunCancelled, releasing
// the single-active-run lock so a new run can start. Returns false if no run is
// active. The actual loop observes ctx cancellation and emits a KindCancelled
// event before unwinding.
func (rt *Runtime) CancelRun(ctx context.Context, sessionID string) (bool, error) {
	rt.mu.Lock()
	rs, ok := rt.runs[sessionID]
	if !ok {
		rt.mu.Unlock()
		return false, nil
	}
	cancel := rs.cancel
	rt.mu.Unlock()

	// Interrupt the in-flight work first so it stops promptly, then settle the
	// terminal state. CompleteRun clears the cancel func.
	if cancel != nil {
		cancel()
	}
	if err := rt.CompleteRun(ctx, sessionID, RunCancelled); err != nil {
		return false, err
	}
	return true, nil
}
