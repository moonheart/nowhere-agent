package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

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

// ToolExecutor dispatches a batch of tool calls at fold time the way the
// run's loop would — each call through the WrapToolCall middleware chain, so
// fold-time execution is governed by the same middleware (redaction, durable
// step intents, ...) as live dispatch. Production wires agent.Loop.ToolExecutor;
// nil falls back to the bare registry's CallAll (tests / middleware-free
// callers). The caller pre-screens the batch (malformed args, schema,
// execution gate) before invoking it.
type ToolExecutor func(ctx context.Context, calls []toolruntime.Call) []toolruntime.Result

// executeFoldCalls routes fold-time dispatch through the loop's middleware
// chain when an executor is wired, falling back to the bare registry
// otherwise. The fallback keeps test and middleware-free callers working, but
// any deployment that registers WrapToolCall middleware MUST pass an executor
// or that middleware silently does not apply to fold-time execution.
func executeFoldCalls(ctx context.Context, exec ToolExecutor, tools *toolruntime.Registry, calls []toolruntime.Call) []toolruntime.Result {
	if exec != nil {
		return exec(ctx, calls)
	}
	return tools.CallAll(ctx, calls)
}

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
// dispatch branch below); pass nil only when the caller has no policy. exec
// routes fold-time tool execution through the loop's WrapToolCall middleware
// chain (agent.Loop.ToolExecutor); pass nil only when no middleware exists.
func (rg *RunRegistry) FoldBatch(ctx context.Context, sessionID, runID string, tools *toolruntime.Registry, gate ToolGate, exec ToolExecutor) ([]provider.Message, error) {
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
	stored, history, err := rg.foldHistory(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("rebuild history: %w", err)
	}

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
	// Execution contract: the fold dispatches through exec — the run loop's
	// WrapToolCall middleware chain (agent.Loop.ToolExecutor) — so fold-time
	// execution is governed by the SAME middleware as live dispatch. This was
	// once a bare Registry.CallAll on the theory that "no middleware
	// implements WrapToolCall"; that claim was false (RedactMW and the
	// durable-accounting toolIntentMW both do), and the bypass let PII/
	// secrets an approved tool echoed land in the durable record unredacted.
	// CallAll remains only as the nil-executor fallback for tests and
	// middleware-free callers (see executeFoldCalls).
	//
	// The loop's other dispatch screens are re-applied here for the same
	// reason: the input-SCHEMA screen (below, alongside the gate) refuses
	// arguments that violate the tool's declared schema, and the EXECUTION
	// gate (the gate parameter) refuses hard-denied calls. "Not gated" only
	// means the call did not need human input — the interaction gate suspends
	// solely on deny-with-approval-marker, ask_user, and client tools, so a
	// HARD-DENIED call (env policy Deny, no approval marker) is an un-gated
	// sibling too. The loop's dispatch screen would have refused it; without
	// the re-check the fold would execute it, making one policy's outcome
	// depend on whether the batch happened to contain an approval-gated
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
			if tool, ok := tools.Get(c.Name); ok {
				// Mirror the loop's dispatch schema screen: arguments that
				// parsed but violate the tool's declared input schema must not
				// execute at fold either — answer with the same structured
				// error dispatch would have produced.
				if verr := toolruntime.ValidateArgs(tool.Schema(), c.Args); verr != nil {
					results[i] = toolruntime.Result{Content: "invalid tool arguments: " + verr.Error(), IsError: true}
					continue
				}
				// Re-apply the execution gate to siblings (see the contract
				// above): a hard-denied call never becomes an interaction, so
				// this is the only screen it gets on the resume path. Mirrors
				// the loop's dispatch: the gate only runs for a resolvable tool
				// (the registry's own guard answers unknown names).
				if gate != nil {
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
		res, err := rg.foldInteraction(ctx, ap, ap.Status == InteractionResolved, tools, exec)
		if err != nil {
			return nil, err
		}
		results[i] = res
	}
	if len(dispatchCalls) > 0 {
		if tools == nil {
			return nil, fmt.Errorf("resuming a suspended batch needs a tool registry to dispatch the un-gated calls")
		}
		got := executeFoldCalls(ctx, exec, tools, dispatchCalls)
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
				_, history, rerr := rg.foldHistory(ctx, sessionID)
				if rerr != nil {
					return nil, fmt.Errorf("rebuild history after concurrent fold: %w", rerr)
				}
				return history, nil
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

// foldHistoryLimit bounds the messages FoldBatch loads to rebuild a resumed
// run's history — the same bound chatapi's rebuildHistoryLimit applies to a
// fresh submission (2000). Both feed the same loop: its per-send
// EnsurePairing repairs a severed tool_use/tool_result boundary and in-loop
// compression fits the view to the context window, so the bound caps the DB
// read and JSON decode, not the model's view. The suspending assistant
// message sits at the very tail of the conversation (the pending gate blocks
// new submissions while a batch awaits a verdict), so the bound cannot cut
// it away in practice; if it ever did, suspendedBatchCalls fails the fold
// loudly instead of folding against a partial record.
const foldHistoryLimit = 2000

// foldHistory loads the session's newest foldHistoryLimit durable messages
// and converts them to provider history, returning both forms (the fold
// locates the suspending assistant message in the stored form). When the
// conversation exceeds the bound, the same truncation marker chatapi's
// rebuildRunHistory prepends is added, so a resumed run and a fresh run
// present identically to the model.
func (rg *RunRegistry) foldHistory(ctx context.Context, sessionID string) ([]StoredMessage, []provider.Message, error) {
	stored, err := rg.msgStore.MessagesTail(ctx, sessionID, 0, foldHistoryLimit+1)
	if err != nil {
		return nil, nil, err
	}
	truncated := len(stored) > foldHistoryLimit
	if truncated {
		stored = stored[len(stored)-foldHistoryLimit:]
	}
	history := StoredMessagesToProvider(stored)
	if truncated {
		history = append([]provider.Message{provider.TextMessage(provider.RoleUser,
			"[Earlier conversation truncated — the beginning of this conversation was not loaded for this run.]")}, history...)
	}
	return stored, history, nil
}

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
func (rg *RunRegistry) Decide(ctx context.Context, approvalID string, approve bool, result json.RawMessage, tools *toolruntime.Registry, gate ToolGate, exec ToolExecutor) (Interaction, []provider.Message, error) {
	ap, complete, err := rg.RecordDecision(ctx, approvalID, approve, result)
	if err != nil {
		return Interaction{}, nil, err
	}
	if !complete {
		return ap, nil, nil
	}
	history, err := rg.FoldBatch(ctx, ap.SessionID, ap.RunID, tools, gate, exec)
	if err != nil {
		return Interaction{}, nil, err
	}
	return ap, history, nil
}
