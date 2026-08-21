package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/observability"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/reqctx"
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
	// TeamID attributes the run to the team whose provider key billed it
	// (enterprise-readiness P1-3); empty means the platform key. Resolved by the
	// transport at submit (chat: the team key Resolve returned; scheduled: the
	// task's team), so exact per-team cost needs no membership join at read time.
	TeamID string
	// Model is the model the loop was configured with, stamped on the run so
	// per-model breakdown and cost estimation need not guess which model ran.
	Model string
}

// RunDoneHook is notified when a run reaches a terminal state (done, failed,
// or cancelled), after the run is settled. It is the integration seam for
// out-of-band consumers of run completion — the webhook notifier, for
// example. Hooks run on their own goroutine with the run's context; they must
// never block or fail the run path, and must tolerate the context being
// cancelled.
type RunDoneHook func(ctx context.Context, sessionID string, run Run, status RunStatus)

// RunRegistry owns run execution. Where Runtime owns run state (the
// single-active-run lock, statuses, the durable log), the registry owns the run's
// context and the goroutine driving the loop, so a run survives the client that
// started it disconnecting. Cancel is transport-independent: any caller can stop
// the run regardless of which HTTP connections are open.
type RunRegistry struct {
	rt *Runtime

	// msgStore, when set, receives the loop's assembled messages (user,
	// assistant, tool-result) for full-block persistence (persist-raw-messages).
	// Nil disables message persistence (tests/dev).
	msgStore MessageStore

	mu      sync.Mutex
	workers map[string]*runWorker // sessionID -> active worker

	// stepLocks serializes run_steps appends per SESSION (map: sessionID ->
	// mutex). The store assigns seq and attempt with MAX+1 subqueries scoped to
	// one run, so only appends for the SAME run can race — a parallel tool
	// batch's concurrent WrapToolCall intents — and the single-active-run
	// constraint makes "per session" == "per run". One lock per session (not
	// one process-wide lock) lets runs of different sessions account steps
	// concurrently. Entries live exactly as long as the session's worker and
	// are removed by the same identity-guarded cleanup (execute).
	stepLocks map[string]*sync.Mutex

	// interactionHandlers maps an interaction Kind to the handler that folds its
	// result into a tool_result on resume (general interrupt). Defaults wire the
	// three built-in kinds; RegisterInteractionHandler adds/overrides kinds.
	interactionHandlers map[string]InteractionHandler

	// doneHooks are notified (async) when a run reaches a terminal state.
	doneHooks []RunDoneHook
}

// runWorker tracks one in-flight run's execution handle.
type runWorker struct {
	runID  string
	cancel context.CancelFunc
	done   chan struct{} // closed when the run goroutine returns
}

// NewRunRegistry creates a registry over a Runtime (state). Lifecycle fan-out
// lives on the Runtime's EventBus (see Runtime.Bus); the registry never held a
// usable bus — it always pointed at the in-memory bus even under a Redis
// broker, so a future misuse would have silently dropped lifecycle events.
func NewRunRegistry(rt *Runtime) *RunRegistry {
	return &RunRegistry{
		rt:                  rt,
		workers:             map[string]*runWorker{},
		stepLocks:           map[string]*sync.Mutex{},
		interactionHandlers: defaultInteractionHandlers(),
	}
}

// RegisterInteractionHandler registers (or overrides) the handler that folds an
// interaction of the given Kind into a tool_result on resume. It returns the
// registry for chaining. A nil handler unregisters the kind.
func (rg *RunRegistry) RegisterInteractionHandler(kind string, h InteractionHandler) *RunRegistry {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if h == nil {
		delete(rg.interactionHandlers, kind)
	} else {
		rg.interactionHandlers[kind] = h
	}
	return rg
}

// WithMessageStore wires full-block message persistence and returns the registry.
func (rg *RunRegistry) WithMessageStore(ms MessageStore) *RunRegistry {
	rg.msgStore = ms
	return rg
}

// WithRunDoneHook registers a hook to be notified — asynchronously, on its own
// goroutine — when a run reaches a terminal state. Hooks run after the run is
// settled and are fire-and-forget: a slow or panicking hook is logged, never
// propagated to the run path. Call before any Submit.
func (rg *RunRegistry) WithRunDoneHook(h RunDoneHook) *RunRegistry {
	rg.doneHooks = append(rg.doneHooks, h)
	return rg
}

// Submit starts a run for the session and executes it on a dedicated goroutine.
// It enforces the single-active-run lock (via Runtime.StartRun) and returns
// ErrRunActive if a run is in flight. The run's context is decoupled from the
// caller's CANCELLATION — the caller disconnecting does not cancel the run —
// but carries the caller's request-scoped values (request id, request-scoped
// logger, user, session id) via reqctx.Detach, so run/loop/tool logs correlate
// back to the request that started the run. ctx is used for the synchronous
// start (session lookup, run row creation).
func (rg *RunRegistry) Submit(ctx context.Context, sessionID string, work RunWork) (Run, error) {
	run, err := rg.rt.StartRun(ctx, sessionID)
	if err != nil {
		return Run{}, err
	}

	// Stamp billing attribution before the loop starts spending (P1-3). This is
	// best-effort — a failed attribution write must not abort a run the user is
	// waiting on — so it logs and continues rather than returning the error.
	if work.TeamID != "" || work.Model != "" {
		if err := rg.rt.store.SetRunAttribution(ctx, run.ID, work.TeamID, work.Model); err != nil {
			slog.Warn("record run attribution", "run", run.ID, "err", err)
		} else {
			run.TeamID, run.Model = work.TeamID, work.Model
		}
	}

	// The worker's context is deliberately decoupled from the caller's request
	// cancellation: the run must outlive the submitting connection (D7).
	// reqctx.Detach is the explicit typed handoff — it re-stamps the caller's
	// request id, logger, user, and session id as reqctx values, so the run's
	// log trail stays correlated with the request that started it without
	// relying on implicit context-value osmosis through WithoutCancel.
	runCtx, cancel := context.WithCancel(reqctx.Detach(ctx))
	w := &runWorker{runID: run.ID, cancel: cancel, done: make(chan struct{})}

	rg.mu.Lock()
	rg.workers[sessionID] = w
	rg.mu.Unlock()

	// Mirror the worker's cancel func into the runtime's run state so the
	// transport-independent paths converge on the same interrupt: CancelRun
	// (used by tests and kept as the runtime-level API) now stops the worker
	// exactly like registry.Cancel does. The runState was created by StartRun
	// above, so registering here is always safe; CompleteRun clears it.
	rg.rt.RegisterCancel(sessionID, cancel)

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
// A run that hits a client interaction (a permission approval, an ask_user
// question set, a client-side tool) is NOT parked: the loop emits KindInterrupt
// and finishes normally, and this run is settled like any other (run-stateless
// model). The interaction's result is applied by a FRESH run that Submit
// installs as a NEW worker — hence the identity-guarded worker removal in the
// deferred cleanup below, so this worker can never clobber its successor.
func (rg *RunRegistry) execute(runCtx context.Context, sessionID string, run Run, w *runWorker, work RunWork) {
	defer close(w.done)
	// Bind a run-scoped logger into the context: the submitter's request logger
	// (carrying request_id) when the run came in over HTTP, else the process
	// default, plus the run/session identity. Loop, middleware, and tool code
	// that logs via observability.LoggerFromContext correlates for free.
	log := observability.LoggerFromContext(runCtx)
	if log == nil {
		log = slog.Default()
	}
	log = log.With("session", sessionID, "run", run.ID)
	runCtx = observability.WithLogger(runCtx, log)
	defer func() {
		rg.mu.Lock()
		// Remove the worker only if it is still the registered one — a new run
		// (a resume after an interaction, or a newer submission) installs a NEW
		// worker for the same session, and this worker's deferred cleanup must
		// not clobber it. The step lock is dropped together with the worker:
		// this run's loop has returned, so no appendStep can still hold it, and
		// the successor worker installs a fresh lock for its own run.
		if rg.workers[sessionID] == w {
			delete(rg.workers, sessionID)
			delete(rg.stepLocks, sessionID)
		}
		rg.mu.Unlock()
	}()
	// A panic anywhere in the loop or its tooling must not crash the process and
	// take every other tenant's run down with it. Recover, settle the run failed,
	// and surface an error frame so attached clients see it end. Declared after
	// the cleanup defers so it runs first (LIFO) and settles before they fire.
	defer func() {
		if p := recover(); p != nil {
			log.Error("run worker panicked", "panic", p, "stack", string(debug.Stack()))
			bg := context.Background()
			errText := fmt.Sprintf("internal error: %v", p)
			rg.voidPendingInteractions(bg, run.ID)
			_ = rg.appendEvent(bg, sessionID, run.ID, agent.KindError, errText)
			// The panic bypasses the emitter, so the error text is passed
			// straight to the message-metadata attach.
			rg.attachRunError(bg, sessionID, run.ID, errText)
			_ = rg.rt.CompleteRunForRun(bg, sessionID, run.ID, RunFailed)
		}
	}()

	pending := &stepIntentQueue{}
	toolMW := &toolIntentMW{rg: rg, sessionID: sessionID, runID: run.ID, pending: pending}
	emit := &registryEmitter{rg: rg, sessionID: sessionID, runID: run.ID, pending: pending, toolMW: toolMW}

	// Install the durable step-intent middlewares: before each provider request
	// and each tool execution, an intent row records the effect with its
	// pre-provisioned result id and durable attempt count (change
	// durable-run-accounting). The loop is per-run, so Use is safe.
	work.Loop.Use(
		&stepIntentMW{rg: rg, sessionID: sessionID, runID: run.ID, pending: pending},
		toolMW,
	)

	// Persist the user turn that started this run as its first message, so the
	// conversation record (the compression/history source) includes the user side.
	// Best-effort: a failure is logged, never fatal — the live stream is the
	// run's real output and must not be aborted by a persist hiccup.
	if rg.msgStore != nil && work.UserMessage != nil {
		if _, err := rg.msgStore.AppendMessage(context.Background(), StoredMessage{
			SessionID: sessionID,
			RunID:     run.ID,
			Role:      work.UserMessage.Role,
			Content:   contextmgmt.TruncateBlocksForPersistence(work.UserMessage.Content),
		}); err != nil {
			log.Warn("persist user message", "role", work.UserMessage.Role, "err", err)
		}
	}

	// Install a subagent activity sink so any spawn_agent tool call in this run
	// forwards its child's progress to the run panel as live-only content events
	// (never persisted). Nested subagents at any depth share this one sink.
	runCtx = subagent.WithSink(runCtx, func(a subagent.Activity) {
		rg.append(runCtx, sessionID, run.ID, agent.KindSubagent, a)
	})

	// Tag the run context with its owning session id so context-resolved policies
	// (the permission middleware's per-session mode) and tools (the subagent spawn
	// tool, which propagates the id to child loops) can read it at call time.
	runCtx = agent.ContextWithSessionID(runCtx, sessionID)

	_, runErr := work.Loop.Run(runCtx, work.History, emit)

	// Determine terminal status; cancelled beats failed when the ctx was cancelled.
	status := RunDone
	if runErr != nil {
		status = RunFailed
		if runCtx.Err() == context.Canceled {
			status = RunCancelled
		}
	}

	// A run that suspended for a client interaction already recorded its durable
	// Interaction row: the emitter persists it synchronously when the loop emits
	// KindInterrupt, BEFORE the data-interaction frame is published (see
	// registryEmitter.persistInteraction). That ordering is load-bearing — a fast
	// client (an instant client-tool auto-run) could otherwise POST its verdict on
	// a row that isn't committed yet and get a 404. Persisting here, after Run()
	// returned, left that resume race open; it now lives on the emit path.

	// The loop emits its terminal content event (KindDone / KindError /
	// KindCancelled) itself — on a cancellation-detached context for the
	// cancelled frame (agent emitCancelled), so the emitter's ctx guard no
	// longer drops it. The registry keeps a compensation for the narrow window
	// where a cancellation lands on a path that never emitted the frame (e.g.
	// racing an error path); it fires only when the emitter did NOT land the
	// cancelled frame, keeping the durable log single-written. Either way the
	// terminal event is persisted BEFORE CompleteRun settles the run (D5),
	// closing the race where an attacher saw the run inactive but no terminal
	// event.
	bg := context.Background()
	// A run that did not end cleanly never waits on client input. If it failed
	// (or was cancelled) mid-way through emitting a gated batch — a KindInterrupt
	// emit error — the rows persisted by the earlier emits would stay pending
	// with no run behind them, and the session's pending-interaction gate would
	// reject every later submission (a permanent 409 no client can resolve).
	// Void the leftovers so the gate clears.
	if runErr != nil {
		rg.voidPendingInteractions(bg, run.ID)
	}
	if status == RunCancelled && !emit.cancelledPersisted.Load() {
		// The terminal event must land: if it fails to persist, attached clients
		// would never see the run end cancelled. Retry once on the same live
		// context; AppendEvent continues the offset from the durable log.
		if err := rg.appendEvent(bg, sessionID, run.ID, agent.KindCancelled, nil); err != nil {
			_ = rg.appendEvent(bg, sessionID, run.ID, agent.KindCancelled, nil)
		}
	}
	// Failed-run visibility (change failed-run-retry): the terminal error is
	// currently only in run_events, which history rebuild never reads — a
	// reloaded client would see the failed run "just stop". Attach the exact
	// error text the live client saw to the run's last assistant message as
	// metadata, so /history can echo it and the UI can offer a retry. The loop
	// emitted its assistant KindMessage before KindError, so the message is
	// durable by the time we get here. Best-effort: a persistence hiccup must
	// not fail the run path.
	if status == RunFailed {
		if errText := emit.terminalErrorText(); errText != "" {
			rg.attachRunError(bg, sessionID, run.ID, errText)
		}
	}
	_ = rg.rt.CompleteRunForRun(bg, sessionID, run.ID, status)

	// Run-completion hooks (webhook notifications and friends): fire each on
	// its own goroutine so a slow consumer can never delay the next run on the
	// session or the registry's teardown. A panicking hook is recovered and
	// logged — out-of-band consumers must not take the run path down.
	for _, hook := range rg.doneHooks {
		hook := hook
		go func() {
			defer func() {
				if p := recover(); p != nil {
					log.Error("run done hook panicked", "panic", p, "session", sessionID, "run", run.ID)
				}
			}()
			hook(runCtx, sessionID, run, status)
		}()
	}
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

// voidPendingInteractions force-resolves (rejects) every still-pending
// interaction a failed or cancelled run left behind. Interactions are persisted
// synchronously on the KindInterrupt emit, so a mid-batch emit failure parks the
// earlier rows with no live run awaiting their verdict — and the session's
// pending-interaction gate would then reject all later submissions. A row
// already decided by a racing client verdict is left as-is.
func (rg *RunRegistry) voidPendingInteractions(ctx context.Context, runID string) {
	pending, err := rg.rt.store.PendingApprovalsForRun(ctx, runID)
	if err != nil {
		slog.Warn("run ended abnormally; could not list leftover pending interactions", "run", runID, "err", err)
		return
	}
	for _, in := range pending {
		if _, err := rg.rt.store.DecideApproval(ctx, in.ID, false, nil); err != nil {
			if errors.Is(err, ErrNoPendingApproval) {
				continue // a client verdict raced us; the row is already resolved
			}
			slog.Warn("run ended abnormally; could not void leftover pending interaction", "run", runID, "interaction", in.ID, "err", err)
		}
	}
	if len(pending) > 0 {
		slog.Info("voided leftover pending interactions of an abnormally-ended run", "run", runID, "count", len(pending))
	}
}

// attachRunError attaches a failed run's terminal error text to its last
// assistant message as metadata {"error": text}, so history rebuild can surface
// the failure to a reloaded client and the UI can offer a retry. It is the
// message-side counterpart to the durable KindError event (which run_events
// carries but history rebuild never reads). Best-effort and failure-tolerant: a
// missing message store, a store error, or a run with no assistant message
// (failed before any output) only logs — the error stays in run_events as
// before. The metadata is never fed back to the model on a later run
// (StoredMessagesToProvider drops it), so it cannot poison conversation
// history.
func (rg *RunRegistry) attachRunError(ctx context.Context, sessionID, runID, errText string) {
	if rg.msgStore == nil {
		return
	}
	// Bounded read: only the failing run's last assistant message is needed,
	// never the whole conversation (LastAssistantMessage is O(1) rows).
	m, err := rg.msgStore.LastAssistantMessage(ctx, sessionID, runID)
	if err != nil {
		slog.Warn("attach run error to message", "session", sessionID, "run", runID, "err", err)
		return
	}
	if m == nil {
		slog.Warn("failed run has no assistant message to carry its error", "session", sessionID, "run", runID)
		return
	}
	meta, merr := json.Marshal(map[string]string{"error": errText})
	if merr != nil {
		return
	}
	if uerr := rg.msgStore.SetMessageMetadata(ctx, m.ID, meta); uerr != nil {
		slog.Warn("attach run error to message", "session", sessionID, "run", runID, "message", m.ID, "err", uerr)
	}
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

// CancelAndWait stops the session's in-flight run like Cancel, then waits up to
// timeout for the worker goroutine to fully exit. A caller about to delete the
// session's rows (purge, account erasure) uses this so the worker's final
// writes land BEFORE the cascade removes its tables — deleting under a live
// worker fails its next write with a bogus FK error at best, and a stale
// CompleteRun could clobber a newer run's state at worst. Returns false if no
// run is active; a worker that does not exit within the timeout is logged and
// left to unwind on its own (the delete proceeds — its writes fail harmlessly
// against the removed rows).
func (rg *RunRegistry) CancelAndWait(sessionID string, timeout time.Duration) bool {
	rg.mu.Lock()
	w, ok := rg.workers[sessionID]
	rg.mu.Unlock()
	if !ok {
		return false
	}
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(timeout):
		slog.Warn("run worker did not exit within the cancel timeout; proceeding without it",
			"session", sessionID, "run", w.runID, "timeout", timeout)
	}
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

// PendingApprovalForSession returns the session's earliest outstanding human
// interaction (the queue head — a pending permission approval or ask_user
// question set), or false. A reloading client uses it to re-render the card the
// transient data-interaction frame showed before the refresh.
func (rg *RunRegistry) PendingApprovalForSession(ctx context.Context, sessionID string) (Approval, bool, error) {
	return rg.rt.store.PendingApprovalForSession(ctx, sessionID)
}

// PendingApprovalsForSession returns the session's full pending interaction
// queue in order (a gated batch parks one interaction per gated call). A
// reloading client re-renders every card from this list, not just the head.
func (rg *RunRegistry) PendingApprovalsForSession(ctx context.Context, sessionID string) ([]Interaction, error) {
	return rg.rt.store.PendingApprovalsForSession(ctx, sessionID)
}

// BatchFoldState reports the recovery state of a run's suspended batch for the
// resume endpoint: whether the fold committed, and how many of the batch's
// interactions are still pending. The combination distinguishes "already
// decided AND folded" (a plain duplicate verdict — reject it) from "decided
// but NOT folded" (a crash/failure between decision and fold — retriable).
func (rg *RunRegistry) BatchFoldState(ctx context.Context, runID string) (folded bool, pending int, err error) {
	snap, err := rg.rt.store.SuspendedBatchForRun(ctx, runID)
	if err != nil && !errors.Is(err, ErrNoSuspendedBatch) {
		return false, 0, err
	}
	// A missing snapshot (pre-snapshot rows) reads as unfolded: the fold will
	// fail loudly downstream, which is the honest answer.
	folded = err == nil && snap.FoldedSeq != nil
	pend, err := rg.rt.store.PendingApprovalsForRun(ctx, runID)
	if err != nil {
		return false, 0, err
	}
	return folded, len(pend), nil
}

// DecidedButUnfoldedRun returns the newest run of the session whose suspended
// batch is fully decided but not yet folded — the crash window between
// RecordDecision and the fold commit. A fresh submission reconciles it (folds
// it) first, so a decided batch's tool_use never dangles in the history the
// new run sends; without this only a client re-sending the same verdict
// repairs the batch. Returns ("", nil) when nothing needs folding. Runs with
// no snapshot, already folded, or still awaiting verdicts are skipped (a
// pending batch is the pending gate's job). "Newest" is by run seq, so the
// scan is independent of the store's row order.
//
// The scan walks runs from newest to oldest. A folded snapshot does NOT stop
// it: the submission-time reconcile is fail-open (a fold failure only logs,
// and the submission proceeds), so a NEWER run's batch can fold while an
// OLDER run's fold crashed and never ran — stopping at that folded snapshot
// would orphan the older decided batch's tool_use forever (returning "" with
// it still decided-but-unfolded). The whole scan costs exactly two queries —
// RunsForSession plus one SuspendedBatchesForSession — plus at most one
// PendingApprovalsForRun probe per run that has an unfolded snapshot: runs
// without a snapshot skip out in memory, folded runs never reach
// PendingApprovalsForRun, and the common case (newest runs batchless) scans
// cheaply.
func (rg *RunRegistry) DecidedButUnfoldedRun(ctx context.Context, sessionID string) (string, error) {
	runs, err := rg.rt.store.RunsForSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	batches, err := rg.rt.store.SuspendedBatchesForSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	byRun := make(map[string]SuspendedBatch, len(batches))
	for _, b := range batches {
		byRun[b.RunID] = b
	}
	slices.SortFunc(runs, func(a, b Run) int { return b.Seq - a.Seq })
	for i := range runs {
		snap, ok := byRun[runs[i].ID]
		if !ok {
			continue // no suspended batch on this run
		}
		if snap.FoldedSeq != nil {
			continue // folded; an older run's fold may still be pending (fail-open reconcile)
		}
		pend, err := rg.rt.store.PendingApprovalsForRun(ctx, runs[i].ID)
		if err != nil {
			return "", err
		}
		if len(pend) > 0 {
			continue // still awaiting verdicts; keep looking older
		}
		return runs[i].ID, nil
	}
	return "", nil
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

// stepLock returns (creating on first use) the per-session lock that
// serializes run_steps appends. The store assigns seq and attempt with MAX+1
// subqueries scoped to one run, so only concurrent appends for the SAME run (a
// parallel tool batch's WrapToolCall intents) can race; the single-active-run
// constraint means a session carries at most one live run, so the session is
// the right serialization key. Entries are removed with the worker in execute.
func (rg *RunRegistry) stepLock(sessionID string) *sync.Mutex {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	l, ok := rg.stepLocks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		rg.stepLocks[sessionID] = l
	}
	return l
}

// appendStep writes one step intent, serializing concurrent appends for the
// same run (parallel tool batches) through the session's lock. Appends of
// different runs proceed concurrently.
func (rg *RunRegistry) appendStep(ctx context.Context, sessionID, runID string, kind StepKind, toolCallID string, sharedID *int64) (RunStep, error) {
	lock := rg.stepLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	return rg.rt.store.AppendRunStep(ctx, runID, kind, toolCallID, sharedID)
}
