package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
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
	// ListUndreamedSessions returns sessions with messages beyond their dreamed
	// watermark (any status — open conversations are learnable). This is the
	// dreaming worker's eligibility scan (incremental model, capability-gap K1).
	ListUndreamedSessions(ctx context.Context) ([]Session, error)
	// DreamedSeq returns a session's dreaming watermark (messages.id consolidated
	// up to); 0 means nothing consolidated yet.
	DreamedSeq(ctx context.Context, id string) (int64, error)
	// MarkDreamedSeq advances the watermark (never backwards), so the worker
	// resumes after it next pass. Idempotent.
	MarkDreamedSeq(ctx context.Context, id string, seq int64) error

	// MemoryInjectedAt returns the session's memory-injection watermark (the
	// moment memories were last surfaced into the conversation). The zero time
	// means nothing injected yet — the first turn injects the full relevant set
	// (incremental injection, capability K / context-mgmt).
	MemoryInjectedAt(ctx context.Context, id string) (time.Time, error)
	// MarkMemoryInjectedAt advances the watermark (never backwards), so the next
	// injection surfaces only memories created after it. Idempotent.
	MarkMemoryInjectedAt(ctx context.Context, id string, at time.Time) error

	// Session state (capability-gap O1, migration 000014): a generic key/value
	// store per session, backing plan/todo and any future session-level state.
	// SetSessionStateKV upserts ONE key (jsonb_set on the DB; it must not clobber
	// sibling keys). value is the raw JSON for that key.
	SetSessionStateKV(ctx context.Context, id, key string, value json.RawMessage) error
	// SessionStateKV returns the JSON value stored under key, or false if unset.
	SessionStateKV(ctx context.Context, id, key string) (json.RawMessage, bool, error)
	// SessionState returns the session's whole state dictionary (for history
	// recovery, which hands every key to the reloading client).
	SessionState(ctx context.Context, id string) (map[string]json.RawMessage, error)

	CreateRun(ctx context.Context, sessionID string, seq int) (Run, error)
	UpdateRunStatus(ctx context.Context, runID string, status RunStatus) error
	// SetRunUsage records the run's aggregate token usage (across all its LLM
	// calls) once the run reports it. u is nil-safe (a no-op).
	SetRunUsage(ctx context.Context, runID string, u *provider.Usage) error
	// ActiveRun returns the active run in a session, or false.
	ActiveRun(ctx context.Context, sessionID string) (Run, bool, error)
	// NextRunSeq returns the next sequence number for a session's run.
	NextRunSeq(ctx context.Context, sessionID string) (int, error)
	// RunsForSession returns all runs in a session, for history replay.
	RunsForSession(ctx context.Context, sessionID string) ([]Run, error)
	// FailStrandedRuns marks every non-terminal run failed. Called at startup to
	// reconcile runs left running by a process that died mid-run, so they no
	// longer read as active. Returns the number of runs updated.
	FailStrandedRuns(ctx context.Context) (int, error)

	AppendEvent(ctx context.Context, e Event) error
	// EventsAfter returns events for a run with offset > after, ordered.
	EventsAfter(ctx context.Context, runID string, after int) ([]Event, error)

	// Tool-approval records (capability-gap O2, migration 000010): the durable
	// thread-level store for a pending human decision.
	// CreateApproval persists a new pending approval. Multiple pendings per
	// session are allowed (multi-approval queue): one per gated call in a batch.
	CreateApproval(ctx context.Context, a Approval) (Approval, error)
	// PendingApprovalForSession returns the session's earliest outstanding
	// approval (the queue head), or false.
	PendingApprovalForSession(ctx context.Context, sessionID string) (Approval, bool, error)
	// PendingApprovalsForSession returns every pending interaction for a session
	// in queue order (earliest first) — the full gated batch.
	PendingApprovalsForSession(ctx context.Context, sessionID string) ([]Interaction, error)
	// PendingApprovalsForRun returns the still-pending interactions of one run
	// (one gated batch), in queue order; empty means the batch is fully resolved.
	PendingApprovalsForRun(ctx context.Context, runID string) ([]Interaction, error)
	// ApprovalsForRun returns ALL interactions of one run (one gated batch), any
	// status, in queue order — used to fold a fully-resolved batch into results.
	ApprovalsForRun(ctx context.Context, runID string) ([]Interaction, error)
	// GetApproval fetches an approval by id (any status).
	GetApproval(ctx context.Context, id string) (Approval, error)
	// DecideApproval resolves a pending approval, or ErrNoPendingApproval. answer
	// is the user's structured response for ask_user (nil for a permission approval).
	DecideApproval(ctx context.Context, id string, approve bool, answer json.RawMessage) (Approval, error)
}

// Runtime coordinates run lifecycle, the single-active-run lock, and event
// fan-out to attached clients. Transport (WS/SSE) subscribes; Runtime owns state.
//
// Delivery is split by latency budget (design redis-stream-live D1): streaming
// content deltas go to a StreamBroker with no database on the path, while
// lifecycle events (running/done/error/cancelled/user) are persisted to the
// durable run event log and fanned out over the EventBus. The durability
// boundary for content is the assembled message (MessageStore), not the token.
type Runtime struct {
	store  Store
	bus    EventBus
	broker StreamBroker

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

// NewRuntime creates a Runtime over a Store, fanning lifecycle events out over
// an in-memory bus and content deltas over an in-memory broker. Use WithBus /
// WithBroker to substitute multi-instance implementations (e.g. Redis).
func NewRuntime(store Store) *Runtime {
	return &Runtime{
		store:  store,
		bus:    NewMemBus(),
		broker: NewMemBroker(0),
		runs:   map[string]*runState{},
	}
}

// WithBus sets the EventBus used for lifecycle fan-out and returns the Runtime.
func (rt *Runtime) WithBus(bus EventBus) *Runtime {
	rt.bus = bus
	return rt
}

// WithBroker sets the StreamBroker used for live content delivery and returns
// the Runtime.
func (rt *Runtime) WithBroker(b StreamBroker) *Runtime {
	rt.broker = b
	return rt
}

// Bus returns the Runtime's EventBus, for transports that subscribe directly.
func (rt *Runtime) Bus() EventBus { return rt.bus }

// Broker returns the Runtime's StreamBroker, for transports that read the live
// content stream directly.
func (rt *Runtime) Broker() StreamBroker { return rt.broker }

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

	// The in-memory lock only covers this process. Consult the durable store so a
	// run started by another instance (or one left active in the DB) still blocks
	// a new one. The partial unique index on runs is the race-free backstop; this
	// check rejects the common, non-racing case cleanly.
	if _, active, err := rt.store.ActiveRun(ctx, sessionID); err != nil {
		return Run{}, err
	} else if active {
		return Run{}, ErrRunActive
	}

	seq, err := rt.store.NextRunSeq(ctx, sessionID)
	if err != nil {
		return Run{}, err
	}
	run, err := rt.store.CreateRun(ctx, sessionID, seq)
	if err != nil {
		// Lost the race to a concurrent writer for this session's active run: the
		// partial unique index (or the seq constraint) rejected the insert.
		// Surface it as the single-active-run error, not a raw DB failure.
		if isActiveRunConflict(err) {
			return Run{}, ErrRunActive
		}
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

// AppendEvent routes one event by kind. Streaming content deltas are published
// to the live broker WITHOUT touching the database (the hot path must not wait
// on a write); lifecycle events are persisted to the durable log and fanned out
// over the bus. This is the single write path for run output.
func (rt *Runtime) AppendEvent(ctx context.Context, e Event) error {
	if isContentKind(e.Kind) {
		_, err := rt.broker.Publish(ctx, e.SessionID, StreamEvent{
			RunID:   e.RunID,
			Kind:    e.Kind,
			Payload: e.Payload,
		})
		return err
	}

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

// isContentKind reports whether an event kind is a streaming content delta that
// belongs on the live broker (not the durable per-token log). Lifecycle kinds
// (running/done/error/cancelled/user) are persisted to run_events instead. The
// interrupt frame is broker-routed too — the run worker persists the durable
// Interaction record separately, and the transient data-interaction frame must
// reach the client live to drive the approval/ask_user/client-tool card.
//
// Usage is broker-routed as well: it's ephemeral stream detail rendered as the
// data-usage frame (and stashed for the finish frame's counts), not a durable
// per-token record. Without this it fell to the lifecycle path, where the
// attacher's lifecycle handler drops it — so live streams reported usage:0 and
// only a history reload (which rebuilds usage from message metadata) showed the
// real counts.
//
// Matched against the agent.EventKind constants (NOT string literals) so a
// rename of a kind's value can't silently desync this routing — as happened when
// "approval_request" became agent.KindInterrupt and this list still checked the
// old string, dropping every interaction frame in the runtime-wired server.
func isContentKind(kind string) bool {
	switch agent.EventKind(kind) {
	case agent.KindText, agent.KindThinking, agent.KindToolUse, agent.KindToolArgs, agent.KindToolResult, agent.KindSubagent, agent.KindInterrupt, agent.KindUsage, agent.KindStepStart, agent.KindStepFinish:
		return true
	default:
		return false
	}
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
	// The run is settling: its live content stream has served its purpose and is
	// scheduled for cleanup (durable content is already in the message store).
	_ = rt.broker.Settle(ctx, sessionID)
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

// MemoryInjectedAt returns the session's memory-injection watermark
// (pass-through to the store).
func (rt *Runtime) MemoryInjectedAt(ctx context.Context, id string) (time.Time, error) {
	return rt.store.MemoryInjectedAt(ctx, id)
}

// MarkMemoryInjectedAt advances the session's memory-injection watermark
// (pass-through to the store; never backwards).
func (rt *Runtime) MarkMemoryInjectedAt(ctx context.Context, id string, at time.Time) error {
	return rt.store.MarkMemoryInjectedAt(ctx, id, at)
}

// SetSessionStateKV persists one key of the session's generic state store and
// fans the change out live (capability-gap O1). The durable write is the source
// of truth (sessions.state via jsonb_set); the live push is a broker StreamEvent
// of kind session_state — a live-only content frame that is NOT written to
// run_events (that table is deprecated), so the state push rides the same fast
// content path as text/tool frames and is re-readable by reconnecting clients
// until the run's stream is cleaned up. Callers (e.g. the plan_write tool) go
// through this so persistence and fan-out stay one write path.
func (rt *Runtime) SetSessionStateKV(ctx context.Context, sessionID, key string, value json.RawMessage) error {
	if err := rt.store.SetSessionStateKV(ctx, sessionID, key, value); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"key": key, "value": value})
	if err != nil {
		return err
	}
	// Tag the frame with the session's active run so attached clients (which
	// filter content by run ID) accept it. A state write usually happens inside a
	// run's tool call, so an active run exists; if there is none (an out-of-band
	// write) the empty RunID still reaches subscribers that don't filter.
	var runID string
	if run, active, err := rt.ActiveRun(ctx, sessionID); err == nil && active {
		runID = run.ID
	}
	_, err = rt.broker.Publish(ctx, sessionID, StreamEvent{RunID: runID, Kind: "session_state", Payload: payload})
	return err
}

// SessionStateKV returns one key of the session's state dictionary (pass-through
// to the store). Call-time readers that need a single setting (e.g. the
// permission middleware resolving permission_mode) use this instead of pulling
// the whole SessionState map on every call.
func (rt *Runtime) SessionStateKV(ctx context.Context, id, key string) (json.RawMessage, bool, error) {
	return rt.store.SessionStateKV(ctx, id, key)
}

// SessionState returns the session's whole state dictionary (pass-through to
// the store), for history recovery.
func (rt *Runtime) SessionState(ctx context.Context, id string) (map[string]json.RawMessage, error) {
	return rt.store.SessionState(ctx, id)
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

// RecoverStrandedRuns marks runs left non-terminal by a previous process as
// failed (startup reconciliation). Single-instance assumption: at startup no
// run is genuinely live, so any non-terminal run is orphaned — its in-memory
// worker died with the process — and would otherwise read as active forever,
// hanging clients that attach to it. Returns the number of runs reconciled.
func (rt *Runtime) RecoverStrandedRuns(ctx context.Context) (int, error) {
	return rt.store.FailStrandedRuns(ctx)
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
