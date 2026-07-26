package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/subagent"
)

// marshalPayload encodes an event payload for the durable log. The run_events
// payload column is jsonb NOT NULL, so a nil payload must encode as JSON null
// ([]byte("null")), not a Go nil slice — which the driver would send as SQL NULL
// and the constraint would reject (dropping lifecycle events like running/done/
// cancelled, which carry no content payload).
func marshalPayload(payload any) []byte {
	if payload == nil {
		return []byte("null")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte("null")
	}
	return data
}

// RunWork carries everything a run worker needs to execute one run: the loop to
// drive, the conversation history, and the system prompt. The registry spawns a
// dedicated goroutine per run so execution is independent of any transport
// connection (design decouple-run-ownership D1).
type RunWork struct {
	Loop    *agent.Loop
	History []provider.Message
	// UserMessage is the new user turn that started this run. When set and a
	// MessageStore is wired, the worker persists it as the run's first message
	// before driving the loop, so the conversation record includes the user side.
	UserMessage *provider.Message
}

// RunRegistry owns run execution. Where Runtime owns run state (the
// single-active-run lock, statuses, the durable log), the registry owns the run's
// context and the goroutine driving the loop, so a run survives the client that
// started it disconnecting. Cancel is transport-independent: any caller can stop
// the run regardless of which HTTP connections are open.
type RunRegistry struct {
	rt  *Runtime
	bus EventBus

	// msgStore, when set, receives the loop's assembled messages (user,
	// assistant, tool-result) for full-block persistence (persist-raw-messages).
	// Nil disables message persistence (tests/dev).
	msgStore MessageStore

	// loopSource, when set, rebuilds a fresh loop for a session — used by Resume
	// to continue a parked run AFTER A PROCESS RESTART, where the original loop
	// (and its tool bindings) is gone (capability-gap O2). Nil confines Resume to
	// the in-process parked work captured at Submit.
	loopSource LoopSource

	mu      sync.Mutex
	workers map[string]*runWorker // sessionID -> active worker
	parked  map[string]RunWork    // runID -> suspended run's work (in-process resume)
}

// LoopSource rebuilds a loop for a session (system prompt + tools bound), so a
// parked run can resume after a restart. The server supplies it from the same
// factory + tool binder used for fresh runs.
type LoopSource func(ctx context.Context, sessionID string) (*agent.Loop, error)

// runWorker tracks one in-flight run's execution handle.
type runWorker struct {
	runID  string
	cancel context.CancelFunc
	done   chan struct{} // closed when the run goroutine returns
}

// NewRunRegistry creates a registry over a Runtime (state) and EventBus (fan-out).
func NewRunRegistry(rt *Runtime, bus EventBus) *RunRegistry {
	return &RunRegistry{rt: rt, bus: bus, workers: map[string]*runWorker{}, parked: map[string]RunWork{}}
}

// WithMessageStore wires full-block message persistence and returns the registry.
func (rg *RunRegistry) WithMessageStore(ms MessageStore) *RunRegistry {
	rg.msgStore = ms
	return rg
}

// WithLoopSource wires the loop factory Resume uses to rebuild a parked run's
// loop after a process restart (capability-gap O2), and returns the registry.
func (rg *RunRegistry) WithLoopSource(ls LoopSource) *RunRegistry {
	rg.loopSource = ls
	return rg
}

// Submit starts a run for the session and executes it on a dedicated goroutine.
// It enforces the single-active-run lock (via Runtime.StartRun) and returns
// ErrRunActive if a run is in flight. The run's context is independent of the
// caller's ctx, so the caller disconnecting does not cancel the run; ctx is used
// only for the synchronous start (session lookup, run row creation).
func (rg *RunRegistry) Submit(ctx context.Context, sessionID string, work RunWork) (Run, error) {
	run, err := rg.rt.StartRun(ctx, sessionID)
	if err != nil {
		return Run{}, err
	}

	// The worker's context is deliberately NOT derived from the caller's request
	// context: the run must outlive the submitting connection (D7).
	runCtx, cancel := context.WithCancel(context.Background())
	w := &runWorker{runID: run.ID, cancel: cancel, done: make(chan struct{})}

	rg.mu.Lock()
	rg.workers[sessionID] = w
	rg.mu.Unlock()

	// Mark the run started so attached/replaying clients learn it began.
	rg.append(runCtx, sessionID, run.ID, agent.KindRunning, nil)

	// Record the user turn as part of run start — before the worker launches — so
	// it deterministically precedes any run output in the durable log. Appended on
	// the run context (not the caller's request context), it also survives the
	// submitter disconnecting immediately after Submit. A fast run could otherwise
	// append its terminal event before the caller recorded the user event.
	if text := messageText(work.UserMessage); text != "" {
		rg.append(runCtx, sessionID, run.ID, agent.KindUser, text)
	}

	go rg.execute(runCtx, sessionID, run, w, work)
	return run, nil
}

// messageText returns the concatenated text of a message's text blocks, or "".
func messageText(m *provider.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == provider.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// execute drives the loop, then settles the run. The terminal lifecycle event is
// persisted and published BEFORE the run is marked settled (D5), so attached
// clients always observe done/failed/cancelled — live or via the replay tail —
// before the run reads as inactive. This removes the settle-before-terminal race.
//
// A run that suspends for tool approval (agent.ErrAwaitingApproval) is NOT
// settled: execute parks it in waiting_approval (releasing the single-active-run
// lock) and returns, leaving the run Active for Resume to continue. The parked
// worker is removed from the registry here so a fresh Submit is not blocked.
func (rg *RunRegistry) execute(runCtx context.Context, sessionID string, run Run, w *runWorker, work RunWork) {
	defer close(w.done)
	defer func() {
		rg.mu.Lock()
		// Remove the worker only if it is still the registered one — a resumed
		// run installs a NEW worker for the same session, and the parked worker's
		// deferred cleanup must not clobber it.
		if rg.workers[sessionID] == w {
			delete(rg.workers, sessionID)
		}
		rg.mu.Unlock()
	}()
	// A panic anywhere in the loop or its tooling must not crash the process and
	// take every other tenant's run down with it. Recover, settle the run failed,
	// and surface an error frame so attached clients see it end. Declared after
	// the cleanup defers so it runs first (LIFO) and settles before they fire.
	defer func() {
		if p := recover(); p != nil {
			slog.Error("run worker panicked", "session", sessionID, "run", run.ID, "panic", p, "stack", string(debug.Stack()))
			bg := context.Background()
			_ = rg.appendEvent(bg, sessionID, run.ID, agent.KindError, fmt.Sprintf("internal error: %v", p))
			_ = rg.rt.CompleteRun(bg, sessionID, RunFailed)
		}
	}()

	emit := &registryEmitter{rg: rg, sessionID: sessionID, runID: run.ID}

	// Persist the user turn that started this run as its first message, so the
	// conversation record (the compression/history source) includes the user side.
	if rg.msgStore != nil && work.UserMessage != nil {
		_, _ = rg.msgStore.AppendMessage(context.Background(), StoredMessage{
			SessionID: sessionID,
			RunID:     run.ID,
			Role:      work.UserMessage.Role,
			Content:   contextmgmt.TruncateBlocksForPersistence(work.UserMessage.Content),
		})
	}

	// Install a subagent activity sink so any spawn_agent tool call in this run
	// forwards its child's progress to the run panel as live-only content events
	// (never persisted). Nested subagents at any depth share this one sink.
	runCtx = subagent.WithSink(runCtx, func(a subagent.Activity) {
		rg.append(runCtx, sessionID, run.ID, agent.KindSubagent, a)
	})

	_, runErr := work.Loop.Run(runCtx, work.History, emit)

	// Human-approval suspension (capability-gap O2): the loop hit a gated tool
	// call and returned ErrAwaitingApproval WITHOUT executing it. Persist the
	// durable Approval and park the run (waiting_approval + release the lock);
	// Resume continues it once the user decides. The run is NOT settled terminal.
	if errors.Is(runErr, agent.ErrAwaitingApproval) {
		rg.mu.Lock()
		rg.parked[run.ID] = work // in-process resume path (pre-restart)
		rg.mu.Unlock()
		rg.suspendForApproval(sessionID, run, work.Loop)
		return
	}

	// Determine terminal status; cancelled beats failed when the ctx was cancelled.
	status := RunDone
	if runErr != nil {
		status = RunFailed
		if runCtx.Err() == context.Canceled {
			status = RunCancelled
		}
	}

	// The loop persists its own terminal content event (KindDone / KindError) on
	// the live run context. The one exception is cancellation: the loop emits
	// KindCancelled on the cancelled runCtx, where the emitter's ctx guard drops
	// it — so the registry re-publishes it on a live context to guarantee the
	// durable log and every attached client see the run end cancelled. This is
	// persisted BEFORE CompleteRun settles the run (D5), closing the race where an
	// attacher saw the run inactive but no terminal event.
	bg := context.Background()
	if status == RunCancelled {
		// The terminal event must land: if it fails to persist, attached clients
		// would never see the run end cancelled. Retry once on the same live
		// context; AppendEvent continues the offset from the durable log.
		if err := rg.appendEvent(bg, sessionID, run.ID, agent.KindCancelled, nil); err != nil {
			_ = rg.appendEvent(bg, sessionID, run.ID, agent.KindCancelled, nil)
		}
	}
	_ = rg.rt.CompleteRun(bg, sessionID, status)
}

// suspendForApproval persists the durable Approval for the loop's gated tool
// call and parks the run in waiting_approval (releasing the single-active-run
// lock). It runs on a background context: the run ctx may already be cancelled.
// The parked run stays Active (waiting_approval) for Resume to continue.
func (rg *RunRegistry) suspendForApproval(sessionID string, run Run, loop *agent.Loop) {
	bg := context.Background()
	gate := loop.PendingApproval
	if gate == nil {
		// Defensive: the loop reported suspension without a gate. Settle failed
		// rather than leave a run parked with no approval to resume from.
		slog.Error("run suspended with no pending approval", "session", sessionID, "run", run.ID)
		_ = rg.rt.CompleteRun(bg, sessionID, RunFailed)
		return
	}
	input, err := json.Marshal(gate.Input)
	if err != nil {
		input = []byte("{}")
	}
	kind := gate.Kind
	if kind == "" {
		kind = "approval"
	}
	ap, err := rg.rt.store.CreateApproval(bg, Approval{
		RunID: run.ID, SessionID: sessionID,
		ToolCallID: gate.ToolCallID, ToolName: gate.ToolName,
		ToolInput: input, Kind: kind,
	})
	if err != nil {
		slog.Error("persist approval failed; failing run", "session", sessionID, "run", run.ID, "err", err)
		_ = rg.rt.CompleteRun(bg, sessionID, RunFailed)
		return
	}
	if _, err := rg.rt.SuspendRun(bg, sessionID); err != nil {
		slog.Error("suspend run failed", "session", sessionID, "run", run.ID, "err", err)
		_ = rg.rt.CompleteRun(bg, sessionID, RunFailed)
		return
	}
	// Surface the interaction request (now carrying the durable id) so an attached
	// client renders the right UI — approve/deny for an approval, a question card
	// for ask_user. Live-only broker content; kind + questions drive the render.
	rg.append(bg, sessionID, run.ID, agent.KindApprovalRequest, map[string]any{
		"approvalId": ap.ID,
		"kind":       kind,
		"toolCallId": ap.ToolCallID,
		"toolName":   ap.ToolName,
		"args":       gate.Input,
	})
	slog.Info("run parked awaiting human input", "session", sessionID, "run", run.ID, "approval", ap.ID, "kind", kind, "tool", ap.ToolName)
}

// Resume continues a run parked in waiting_approval after the user resolved its
// human interaction. approve/answer carry the verdict: a permission approval uses
// approve (execute on true, deny on false); an ask_user question set uses answer
// (the user's structured response) with approve=true, or approve=false to skip.
// It re-acquires the single-active-run lock, rebuilds history from the durable
// message store (which may now include interleaved runs), and drives the loop to
// completion on a fresh worker. The row is decided atomically; a stale/foreign
// decision errors.
func (rg *RunRegistry) Resume(ctx context.Context, approvalID string, approve bool, answer json.RawMessage) (Run, error) {
	ap, err := rg.rt.store.DecideApproval(ctx, approvalID, approve, answer)
	if err != nil {
		return Run{}, err // ErrNoPendingApproval for unknown/already-decided
	}
	sessionID := ap.SessionID

	// Rebuild the durable conversation so the resumed loop sees the full record —
	// including any run the user interleaved while this one was parked (the lock
	// was released at suspend). The approved/denied tool result is NOT yet in the
	// store; the loop's Config.Approval resolves it as the resumed run's first act.
	var history []provider.Message
	if rg.msgStore != nil {
		stored, err := rg.msgStore.MessagesFor(ctx, sessionID)
		if err != nil {
			return Run{}, fmt.Errorf("rebuild history: %w", err)
		}
		history = StoredMessagesToProvider(stored)
	}

	// Resolve the loop to drive. Prefer the in-process parked work (the run
	// suspended in THIS process, so its loop is still warm); after a restart the
	// parked map is empty, so rebuild a fresh loop from the durable state via the
	// loop source — this is what makes Resume survive a process restart.
	rg.mu.Lock()
	work, warm := rg.parked[ap.RunID]
	delete(rg.parked, ap.RunID)
	rg.mu.Unlock()
	var loop *agent.Loop
	if warm {
		loop = work.Loop
	} else {
		if rg.loopSource == nil {
			return Run{}, errors.New("run parked before restart and no loop source configured")
		}
		loop, err = rg.loopSource(ctx, sessionID)
		if err != nil {
			return Run{}, fmt.Errorf("rebuild loop: %w", err)
		}
	}
	var input map[string]any
	_ = json.Unmarshal(ap.ToolInput, &input)
	var answerMap map[string]any
	_ = json.Unmarshal(ap.Answer, &answerMap)
	loop = loop.WithApproval(agent.ResumedApproval{
		Kind: ap.Kind, ToolCallID: ap.ToolCallID, ToolName: ap.ToolName,
		Input: input, Approved: approve, Answer: answerMap,
	})

	run, err := rg.rt.ResumeRun(ctx, sessionID, ap.RunID)
	if err != nil {
		return Run{}, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	w := &runWorker{runID: run.ID, cancel: cancel, done: make(chan struct{})}
	rg.mu.Lock()
	rg.workers[sessionID] = w
	rg.mu.Unlock()

	rg.append(runCtx, sessionID, run.ID, agent.KindRunning, nil)
	go rg.execute(runCtx, sessionID, run, w, RunWork{Loop: loop, History: history})
	return run, nil
}

// appendEvent is append but surfaces the persistence error (for the terminal
// event, where a silent drop means attached clients never see the run end).
func (rg *RunRegistry) appendEvent(ctx context.Context, sessionID, runID string, kind agent.EventKind, payload any) error {
	return rg.rt.AppendEvent(ctx, Event{
		RunID:     runID,
		SessionID: sessionID,
		Kind:      string(kind),
		Payload:   marshalPayload(payload),
	})
}

// Cancel stops the session's in-flight run: it invokes the worker's cancel func
// (interrupting the loop + tools) regardless of any open transport connection.
// The worker goroutine observes the cancellation and settles the run cancelled.
// Returns false if no run is active.
func (rg *RunRegistry) Cancel(sessionID string) bool {
	rg.mu.Lock()
	w, ok := rg.workers[sessionID]
	rg.mu.Unlock()
	if !ok {
		return false
	}
	w.cancel()
	return true
}

// ActiveWorker reports whether a run is currently executing for the session.
func (rg *RunRegistry) ActiveWorker(sessionID string) bool {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	_, ok := rg.workers[sessionID]
	return ok
}

// ApprovalByID fetches an approval record (any status) so the decision endpoint
// can resolve its session for an ownership check before Resume.
func (rg *RunRegistry) ApprovalByID(ctx context.Context, id string) (Approval, error) {
	return rg.rt.store.GetApproval(ctx, id)
}

// PendingApprovalForSession returns the session's outstanding human interaction
// (a parked permission approval or ask_user question set), or false. A reloading
// client uses it to re-render the card the transient data-tool-approval frame
// showed before the refresh. There is at most one pending approval per run and
// at most one waiting_approval run per session (single-active-run), so the
// latest parked run's approval is the session's current interaction.
func (rg *RunRegistry) PendingApprovalForSession(ctx context.Context, sessionID string) (Approval, bool, error) {
	run, active, err := rg.rt.ActiveRun(ctx, sessionID)
	if err != nil || !active || run.Status != RunWaitingApproval {
		return Approval{}, false, err
	}
	return rg.rt.store.PendingApprovalForRun(ctx, run.ID)
}

// append persists an event through the Runtime (which fans it out to subscribers
// on the bus) so the durable log and live stream stay in one write path.
func (rg *RunRegistry) append(ctx context.Context, sessionID, runID string, kind agent.EventKind, payload any) {
	data := marshalPayload(payload)
	_ = rg.rt.AppendEvent(ctx, Event{
		RunID:     runID,
		SessionID: sessionID,
		Kind:      string(kind),
		Payload:   data,
	})
}

// registryEmitter adapts agent.Emitter to the registry's persist+publish write
// path. The loop emits through it; each event is persisted to the durable log and
// fanned out to attached clients by the Runtime's AppendEvent.
type registryEmitter struct {
	rg        *RunRegistry
	sessionID string
	runID     string
}

// Emit persists the event (and fans it out). It honours ctx cancellation so a
// cancelled run unwinds rather than blocking on a dead write path.
func (e *registryEmitter) Emit(ctx context.Context, kind agent.EventKind, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Assembled conversation messages go to the MessageStore for full-block
	// persistence. They are NOT written to run_events: KindMessage is a
	// persistence signal, not a render frame, and the durable run log already
	// carries the text/thinking/tool frames the UI replays.
	if kind == agent.KindMessage {
		e.persistMessage(ctx, payload)
		return nil
	}
	e.rg.append(ctx, e.sessionID, e.runID, kind, payload)
	return nil
}

// persistMessage stores one assembled message in the MessageStore. The payload
// is the provider.Message the loop emitted. Persistence is best-effort: a
// failure is logged but does not abort the run (the run's render stream is
// unaffected).
func (e *registryEmitter) persistMessage(ctx context.Context, payload any) {
	if e.rg.msgStore == nil {
		return
	}
	msg, ok := payload.(provider.Message)
	if !ok {
		// Tolerate a marshalled-then-decoded message (map shape) too.
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
	}
	// Use a background context: by the time the final assistant message is
	// emitted the run ctx may already be cancelled, and we still want the
	// message persisted (mirrors the terminal-event re-publish on a live ctx).
	_, _ = e.rg.msgStore.AppendMessage(context.Background(), StoredMessage{
		SessionID: e.sessionID,
		RunID:     e.runID,
		Role:      msg.Role,
		Content:   contextmgmt.TruncateBlocksForPersistence(msg.Content),
	})
}
