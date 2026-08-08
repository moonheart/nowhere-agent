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

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/subagent"
	"nowhere-agent/internal/toolruntime"
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

	mu      sync.Mutex
	workers map[string]*runWorker // sessionID -> active worker

	// interactionHandlers maps an interaction Kind to the handler that folds its
	// result into a tool_result on resume (general interrupt). Defaults wire the
	// three built-in kinds; RegisterInteractionHandler adds/overrides kinds.
	interactionHandlers map[string]InteractionHandler
}

// runWorker tracks one in-flight run's execution handle.
type runWorker struct {
	runID  string
	cancel context.CancelFunc
	done   chan struct{} // closed when the run goroutine returns
}

// NewRunRegistry creates a registry over a Runtime (state) and EventBus (fan-out).
func NewRunRegistry(rt *Runtime, bus EventBus) *RunRegistry {
	return &RunRegistry{
		rt:                  rt,
		bus:                 bus,
		workers:             map[string]*runWorker{},
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

// RecordDecision applies the client's verdict to ONE pending interaction and
// reports whether its batch (the run's interactions) is now fully resolved. It
// only marks the row decided — it does NOT start a run or fold any tool_result.
// batchComplete=false means siblings are still pending: the conversation keeps
// waiting (the model is NOT re-invoked on a partial batch). Only when
// batchComplete=true should the caller start a fresh run via FoldBatch — the
// LangGraph pattern that guarantees the model always sees a complete
// tool_use→tool_result set, never a half-decided batch.
func (rg *RunRegistry) RecordDecision(ctx context.Context, approvalID string, approve bool, result json.RawMessage) (Interaction, bool, error) {
	ap, err := rg.rt.store.DecideApproval(ctx, approvalID, approve, result)
	if err != nil {
		return Interaction{}, false, err // ErrNoPendingApproval for unknown/already-decided
	}
	pending, err := rg.rt.store.PendingApprovalsForRun(ctx, ap.RunID)
	if err != nil {
		return Interaction{}, false, fmt.Errorf("check batch pending: %w", err)
	}
	return ap, len(pending) == 0, nil
}

// ToolGate authorizes one tool at fold time — the same policy the loop's
// dispatch screen (gateExecute) applies, threaded into the fold so the two
// execution paths agree. (deny, reason) blocks the call; the reason is folded
// back as an is_error result, mirroring the loop's "permission denied" result.
// Nil means no gate (tests / policy-free deployments).
type ToolGate func(ctx context.Context, tool toolruntime.Tool) (deny bool, reason string)

// FoldBatch folds every interaction of one run (one gated batch) into a single
// user message carrying each call's tool_result, persists it, and returns the
// rebuilt history for a FRESH run to continue the conversation. Call it only
// after RecordDecision reports the batch complete. Each interaction is folded by
// its Kind's registered InteractionHandler (see interaction.go):
//   - tool_approval approved → the tool is EXECUTED now and its real result fed
//     back; rejected → an is_error denial;
//   - ask_user answered → the structured answers; skipped → a "skipped" note;
//   - client_tool → the client's output (validated) or an is_error.
//
// The batch is resolved from the run's durable suspended-batch snapshot
// (capability suspend-batch-snapshot), NEVER from a session-wide history scan:
// the suspending assistant message is located by run ID and its tool_use ID set
// must equal the snapshot's, or the fold fails without executing anything. The
// fold commits atomically (FoldCommitter) and is idempotent — an already-folded
// batch re-executes nothing and returns the rebuilt history.
//
// tools may be nil only when no fold can require execution (all rejected /
// ask_user / skipped). gate re-authorizes the un-gated sibling calls (see the
// dispatch branch below); pass nil only when the caller has no policy.
func (rg *RunRegistry) FoldBatch(ctx context.Context, sessionID, runID string, tools *toolruntime.Registry, gate ToolGate) ([]provider.Message, error) {
	// The fold is the suspended batch's durable completion — a commit-class
	// operation, not request-scoped work. Detach it from the caller's
	// cancellation: a client disconnect after POSTing the final verdict must
	// not abort tool execution halfway and strand the batch decided-but-
	// unfolded (the decision already committed; there is no automatic retry —
	// a decided row renders no pending card). This mirrors the run model,
	// where a run's ctx derives from context.Background, not the submitter's
	// connection. Per-tool timeouts still bound each call; ctx VALUES (the
	// session id the execution gate resolves its mode from) are preserved.
	ctx = context.WithoutCancel(ctx)
	snap, err := rg.rt.store.SuspendedBatchForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("fold batch: %w", err)
	}
	batch, err := rg.rt.store.ApprovalsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read batch: %w", err)
	}
	if len(batch) == 0 {
		return nil, fmt.Errorf("no interactions for run %s", runID)
	}
	if rg.msgStore == nil {
		return nil, fmt.Errorf("fold batch: a message store is required")
	}
	stored, err := rg.msgStore.MessagesFor(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("rebuild history: %w", err)
	}
	history := StoredMessagesToProvider(stored)

	// Idempotent resume: a committed fold (tool_result message persisted, marker
	// set in the same transaction) never re-executes — a retried resume just
	// returns the rebuilt history.
	if snap.FoldedSeq != nil {
		return history, nil
	}

	// Fold the WHOLE suspended tool_use batch, not just the calls that parked an
	// interaction. The run ended on the unified gate the moment ANY call was gated
	// (agent loop), so its sibling calls in the same batch — the ones that needed no
	// approval — were never dispatched and have no tool_result. Answer every call:
	// a call with an interaction folds its verdict; a call without one is
	// dispatched now, so the model never sees a dangling tool_use.
	allCalls, err := suspendedBatchCalls(stored, runID, snap)
	if err != nil {
		return nil, err
	}
	byCall := make(map[string]Interaction, len(batch))
	for _, ap := range batch {
		byCall[ap.ToolCallID] = ap
	}

	// Fold each call into its tool_result, preserving batch order (the tool_use
	// order in the assistant message). Dispatch the no-interaction calls together
	// (concurrently, mirroring the loop's dispatch) so a resumed batch of several
	// plain reads doesn't serialize.
	//
	// Execution contract: the fold dispatches through the bare tool registry
	// (CallAll), NOT the loop's chainTool wrap chain — safe because no middleware
	// implements WrapToolCall and CallAll replicates the loop's innermost
	// realToolCall exactly (call-id ctx for progress nesting, per-tool timeout,
	// unknown-tool guard). If a WrapToolCall middleware is ever added, it must be
	// routed into the fold too — add an executor hook here rather than calling
	// the registry directly.
	//
	// The EXECUTION gate, by contrast, is re-applied here (the gate parameter):
	// "not gated" only means the call did not need human input — the interaction
	// gate suspends solely on deny-with-approval-marker, ask_user, and client
	// tools, so a HARD-DENIED call (env policy Deny, no approval marker) is an
	// un-gated sibling too. The loop's dispatch screen would have refused it;
	// without the re-check the fold would execute it, making one policy's
	// outcome depend on whether the batch happened to contain an approval-gated
	// neighbour. Gated calls skip the re-check: the human verdict supersedes
	// the ask-tier (the env tier is static config, unchanged since suspend).
	resultMsg := provider.Message{Role: provider.RoleUser}
	results := make([]toolruntime.Result, len(allCalls))
	var dispatchIdx []int
	var dispatchCalls []toolruntime.Call
	for i, c := range allCalls {
		if c.ArgsError != "" {
			// Mirror the loop's dispatch screen: a call whose arguments never
			// parsed was refused at the gate too (no interaction row), and it
			// must not execute at fold either — its ToolInput is nil/partial.
			results[i] = toolruntime.Result{Content: "invalid tool arguments: " + c.ArgsError, IsError: true}
			continue
		}
		ap, gated := byCall[c.ID]
		if !gated {
			// Re-apply the execution gate to siblings (see the contract above):
			// a hard-denied call never becomes an interaction, so this is the
			// only screen it gets on the resume path. Mirrors the loop's
			// dispatch: the gate only runs for a resolvable tool (the registry's
			// own guard answers unknown names).
			if gate != nil {
				if tool, ok := tools.Get(c.Name); ok {
					if deny, reason := gate(ctx, tool); deny {
						results[i] = toolruntime.Result{Content: "permission denied: " + reason, IsError: true}
						continue
					}
				}
			}
			dispatchIdx = append(dispatchIdx, i)
			dispatchCalls = append(dispatchCalls, c)
			continue
		}
		res, err := rg.foldInteraction(ctx, ap, ap.Status == InteractionResolved, tools)
		if err != nil {
			return nil, err
		}
		results[i] = res
	}
	if len(dispatchCalls) > 0 {
		if tools == nil {
			return nil, fmt.Errorf("resuming a suspended batch needs a tool registry to dispatch the un-gated calls")
		}
		got := tools.CallAll(ctx, dispatchCalls)
		for j, i := range dispatchIdx {
			results[i] = got[j]
		}
	}
	for i, c := range allCalls {
		content := results[i].Content
		if content == "" {
			content = emptyToolResultPlaceholder
		}
		resultMsg.Content = append(resultMsg.Content, provider.Block{
			Type:         provider.BlockToolResult,
			ToolResultID: c.ID,
			ToolContent:  content,
			IsError:      results[i].IsError,
			ToolMessages: results[i].Nested,
		})
	}

	// Commit the fold: the suspended run recorded the assistant tool_use batch
	// but never its results (it ended at the gate). Persist the folded
	// tool_results and mark the batch folded — atomically where the store
	// supports it (PG), sequentially otherwise (mem, tests/dev). A failure is
	// returned so the client retries the resume; the folded_seq marker keeps a
	// retry from re-executing once the commit did land.
	storedMsg := StoredMessage{
		SessionID: sessionID,
		RunID:     runID,
		Role:      resultMsg.Role,
		Content:   contextmgmt.TruncateBlocksForPersistence(resultMsg.Content),
	}
	if fc, ok := rg.rt.store.(FoldCommitter); ok {
		if _, err := fc.CommitFold(context.Background(), runID, storedMsg); err != nil {
			if errors.Is(err, ErrBatchAlreadyFolded) {
				// A concurrent fold of this batch committed first; treat as
				// idempotent success (the tool_result message is persisted).
				stored, rerr := rg.msgStore.MessagesFor(ctx, sessionID)
				if rerr != nil {
					return nil, fmt.Errorf("rebuild history after concurrent fold: %w", rerr)
				}
				return StoredMessagesToProvider(stored), nil
			}
			return nil, fmt.Errorf("commit fold: %w", err)
		}
	} else {
		appended, err := rg.msgStore.AppendMessage(context.Background(), storedMsg)
		if err != nil {
			return nil, fmt.Errorf("persist fold message: %w", err)
		}
		if err := rg.rt.store.MarkBatchFolded(context.Background(), runID, appended.Seq); err != nil {
			return nil, fmt.Errorf("mark batch folded: %w", err)
		}
	}
	history = append(history, resultMsg)
	return history, nil
}

// emptyToolResultPlaceholder stands in for a folded interaction whose result is
// empty, so the serialized tool message always carries a non-empty content field.
const emptyToolResultPlaceholder = "(no output)"

// suspendedBatchCalls resolves the suspended batch's calls from the durable
// record: the last tool_use-bearing assistant message persisted under runID
// (scoping by run makes cross-run pollution structurally impossible), validated
// against the snapshot — the message's tool_use IDs, in block order, MUST equal
// the snapshot's recorded IDs. Any mismatch fails the fold loudly: nothing
// executes, nothing persists.
func suspendedBatchCalls(stored []StoredMessage, runID string, snap SuspendedBatch) ([]toolruntime.Call, error) {
	for i := len(stored) - 1; i >= 0; i-- {
		m := stored[i]
		if m.Role != provider.RoleAssistant || m.RunID != runID {
			continue
		}
		var calls []toolruntime.Call
		var ids []string
		for _, b := range m.Content {
			if b.Type != provider.BlockToolUse || b.ToolUseID == "" {
				continue
			}
		ids = append(ids, b.ToolUseID)
		calls = append(calls, toolruntime.Call{ID: b.ToolUseID, Name: b.ToolName, Args: b.ToolInput, ArgsError: b.ArgsError})
		}
		if len(calls) == 0 {
			continue
		}
		if !slices.Equal(ids, snap.ToolCallIDs) {
			return nil, fmt.Errorf("fold batch: suspended message tool_use IDs %v do not match the snapshot %v (run %s)", ids, snap.ToolCallIDs, runID)
		}
		return calls, nil
	}
	return nil, fmt.Errorf("fold batch: no tool_use-bearing assistant message for run %s", runID)
}

// Decide applies the client's resolution of a pending Interaction and returns
// the history for a FRESH run to continue the conversation (run-stateless model,
// general interrupt). It is the single-interaction convenience wrapper over
// RecordDecision + FoldBatch, kept for the common one-gated-call case: it marks
// the row decided and, when the batch is complete (the usual case for a single
// gated call), folds the batch and returns the resume history. When siblings are
// still pending it returns nil history — the caller must not resume yet.
func (rg *RunRegistry) Decide(ctx context.Context, approvalID string, approve bool, result json.RawMessage, tools *toolruntime.Registry, gate ToolGate) (Interaction, []provider.Message, error) {
	ap, complete, err := rg.RecordDecision(ctx, approvalID, approve, result)
	if err != nil {
		return Interaction{}, nil, err
	}
	if !complete {
		return ap, nil, nil
	}
	history, err := rg.FoldBatch(ctx, ap.SessionID, ap.RunID, tools, gate)
	if err != nil {
		return Interaction{}, nil, err
	}
	return ap, history, nil
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
	// The interrupt frame carries the interaction id the client POSTs its verdict
	// with. Persist the durable Interaction row BEFORE publishing the frame, so a
	// fast client (an instant client-tool auto-run) can never learn an id it can't
	// yet resolve (which would 404 on resume). Synchronous, and ahead of the
	// append below: once any client can see the frame, the row exists. A failure
	// is returned so the loop fails the run instead of surfacing a phantom prompt.
	if kind == agent.KindInterrupt {
		if err := e.persistInteraction(payload); err != nil {
			return err
		}
	}
	// The run's aggregate usage is recorded on the runs row (SetRunUsage is
	// nil-safe and best-effort); the event still flows to the durable log below.
	if kind == agent.KindUsage {
		e.persistRunUsage(payload)
	}
	e.rg.append(ctx, e.sessionID, e.runID, kind, payload)
	return nil
}

// persistInteraction writes the durable Interaction row for a KindInterrupt
// frame BEFORE the frame is published, closing the resume race (a client must
// never learn an interaction id it cannot yet resolve). The payload is the
// loop's agent.Interaction; its id was generated when the gate was detected, so
// it matches the id the frame carries. A persistence failure is returned so the
// emit — and thus the run — fails, rather than a prompt reaching the client with
// no backing row.
func (e *registryEmitter) persistInteraction(payload any) error {
	var in agent.Interaction
	switch v := payload.(type) {
	case agent.Interaction:
		in = v
	case *agent.Interaction:
		if v == nil {
			return fmt.Errorf("interrupt payload is a nil *agent.Interaction")
		}
		in = *v
	default:
		return fmt.Errorf("interrupt payload is not an agent.Interaction: %T", payload)
	}
	input, err := json.Marshal(in.Input)
	if err != nil {
		input = []byte("{}")
	}
	kind := in.Kind
	if kind == "" {
		kind = "approval"
	}
	// The suspended-batch snapshot: the full ordered batch the gate suspended on,
	// persisted in the same transaction as the first interaction row (idempotent
	// across the batch's frames). A hand-constructed payload without Batch
	// degenerates to the single gated call.
	batchIDs := make([]string, 0, len(in.Batch))
	for _, c := range in.Batch {
		if c.ID != "" {
			batchIDs = append(batchIDs, c.ID)
		}
	}
	if len(batchIDs) == 0 && in.ToolCallID != "" {
		batchIDs = []string{in.ToolCallID}
	}
	ap, err := e.rg.rt.store.CreateInteractionBatch(context.Background(), SuspendedBatch{
		RunID: e.runID, SessionID: e.sessionID, ToolCallIDs: batchIDs,
	}, Interaction{
		ID: in.ID, RunID: e.runID, SessionID: e.sessionID,
		ToolCallID: in.ToolCallID, ToolName: in.ToolName,
		Payload: input, Kind: kind,
	})
	if err != nil {
		slog.Error("persist interaction failed; failing run", "session", e.sessionID, "run", e.runID, "err", err)
		return err
	}
	slog.Info("run ended awaiting client interaction", "session", e.sessionID, "run", e.runID, "interaction", ap.ID, "kind", kind, "tool", ap.ToolName)
	return nil
}

// persistRunUsage extracts the run's aggregate usage from a KindUsage payload
// and records it on the runs row. Best-effort: failures are logged, not fatal.
func (e *registryEmitter) persistRunUsage(payload any) {
	var u *provider.Usage
	switch v := payload.(type) {
	case provider.Usage:
		u = &v
	case *provider.Usage:
		u = v
	}
	if u == nil {
		return
	}
	if err := e.rg.rt.store.SetRunUsage(context.Background(), e.runID, u); err != nil {
		slog.Warn("record run usage", "run", e.runID, "err", err)
	}
}

// persistMessage stores one assembled message in the MessageStore. The payload
// is either an agent.MessageWithUsage (an assistant message paired with the
// usage of the LLM call that produced it) or a bare provider.Message (a tool
// result, which is not an LLM call and has no usage). Persistence is
// best-effort: a failure is logged but does not abort the run (the run's render
// stream is unaffected).
func (e *registryEmitter) persistMessage(ctx context.Context, payload any) {
	if e.rg.msgStore == nil {
		return
	}
	var msg provider.Message
	var usage *provider.Usage
	switch v := payload.(type) {
	case agent.MessageWithUsage:
		msg, usage = v.Message, v.Usage
	case provider.Message:
		msg = v
	default:
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
		Usage:     usage,
	})
}
