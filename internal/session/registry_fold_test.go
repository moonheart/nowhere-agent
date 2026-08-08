package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// countTool records how many times it executed.
type countTool struct {
	name string
	runs *int
}

func (c countTool) Name() string           { return c.name }
func (c countTool) Description() string    { return "counting" }
func (c countTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (c countTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }
func (c countTool) Timeout() time.Duration { return time.Second }
func (c countTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	*c.runs++
	return toolruntime.Result{Content: c.name + " done"}, nil
}

// TestSuspendPersistsBatchSnapshot (task 2.4): a run suspending on a gated
// batch durably records ONE snapshot carrying the FULL ordered batch.
func TestSuspendPersistsBatchSnapshot(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	rg := NewRunRegistry(rt, rt.Bus()).WithMessageStore(NewMemMessageStore())
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	loop := agent.New(&twoGatedProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100})
	loop.RegisterTool(gatedTool{name: "edit_a"})
	loop.RegisterTool(gatedTool{name: "edit_b"})
	loop.Use(&agent.PermissionMW{Check: func(context.Context, toolruntime.Tool) (bool, string) {
		return true, agent.ApprovalReasonPrefix + "ask"
	}})
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Fatalf("gated run status = %v, want done (parked)", got)
	}

	snap, err := rg.rt.store.SuspendedBatchForRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("SuspendedBatchForRun: %v", err)
	}
	if len(snap.ToolCallIDs) != 2 || snap.ToolCallIDs[0] != "tu1" || snap.ToolCallIDs[1] != "tu2" {
		t.Errorf("snapshot IDs = %v, want [tu1 tu2] in batch order", snap.ToolCallIDs)
	}
	if snap.FoldedSeq != nil {
		t.Errorf("FoldedSeq = %v, want nil before any fold", *snap.FoldedSeq)
	}
}

// TestFoldBatchIgnoresNewerRuns is the regression test for the reported race: a
// new run appended its own tool_use-bearing messages while the interaction hung
// pending; folding the OLD run must fold the OLD batch (snapshot-driven), never
// re-dispatch the newer run's already-executed calls.
func TestFoldBatchIgnoresNewerRuns(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ctx := context.Background()

	// Run A suspends on a mixed batch: tu_g (danger, gated) + tu_p (load_skill,
	// ungated sibling, never dispatched).
	runA, _ := rg.rt.store.CreateRun(ctx, sess.ID, 1)
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: runA.ID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "old turn"}},
	})
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: runA.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "tu_g", ToolName: "danger", ToolInput: map[string]any{}},
			{Type: provider.BlockToolUse, ToolUseID: "tu_p", ToolName: "load_skill", ToolInput: map[string]any{}},
		},
	})
	ap := createSuspendedInteraction(t, rg, []string{"tu_g", "tu_p"}, Interaction{
		RunID: runA.ID, SessionID: sess.ID, ToolCallID: "tu_g", ToolName: "danger",
		Payload: json.RawMessage(`{}`), Kind: KindToolApproval,
	})

	// While the approval hangs, a NEW run B appends its own turn with its own
	// (already executed) tool_use. The legacy history scan would find THIS
	// message and re-dispatch tu_new.
	runB, _ := rg.rt.store.CreateRun(ctx, sess.ID, 2)
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: runB.ID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "new turn while pending"}},
	})
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: runB.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "tu_new", ToolName: "load_skill", ToolInput: map[string]any{}},
		},
	})
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: runB.ID, Role: provider.RoleUser,
		Content: []provider.Block{
			{Type: provider.BlockToolResult, ToolResultID: "tu_new", ToolContent: "already ran"},
		},
	})

	dangerRuns, readRuns := 0, 0
	reg := toolruntime.NewRegistry()
	reg.Register(countTool{name: "danger", runs: &dangerRuns})
	reg.Register(countTool{name: "load_skill", runs: &readRuns})

	_, history, err := rg.Decide(ctx, ap.ID, true, nil, reg, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dangerRuns != 1 {
		t.Errorf("danger executed %d times, want 1 (the approved gated call)", dangerRuns)
	}
	if readRuns != 1 {
		t.Errorf("load_skill executed %d times, want 1 (run A's ungated sibling ONLY — run B's tu_new must not re-dispatch)", readRuns)
	}
	last := history[len(history)-1]
	if len(last.Content) != 2 || last.Content[0].ToolResultID != "tu_g" || last.Content[1].ToolResultID != "tu_p" {
		t.Errorf("folded results = %+v, want [tu_g tu_p] from run A's batch", last.Content)
	}
	for _, b := range last.Content {
		if b.ToolResultID == "tu_new" {
			t.Error("the fold answered run B's tu_new — wrong batch")
		}
	}
}

// TestFoldBatchSnapshotMismatch: the located message's tool_use IDs must equal
// the snapshot; a mismatch fails loudly and executes/persists nothing.
func TestFoldBatchSnapshotMismatch(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ctx := context.Background()
	run, _ := rg.rt.store.CreateRun(ctx, sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "danger", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu-DIFFERENT"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "danger", Kind: KindToolApproval,
	})

	dangerRuns := 0
	reg := toolruntime.NewRegistry()
	reg.Register(countTool{name: "danger", runs: &dangerRuns})

	before, _ := ms.MessagesFor(ctx, sess.ID)
	_, _, err := rg.Decide(ctx, ap.ID, true, nil, reg, nil)
	if err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("Decide = %v, want a snapshot-mismatch error", err)
	}
	if dangerRuns != 0 {
		t.Error("a mismatched fold must execute nothing")
	}
	after, _ := ms.MessagesFor(ctx, sess.ID)
	if len(after) != len(before) {
		t.Error("a mismatched fold must persist nothing")
	}
}

// TestFoldBatchMissingSnapshot: interactions without a snapshot row fail the
// fold with ErrNoSuspendedBatch — no heuristic fallback.
func TestFoldBatchMissingSnapshot(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ctx := context.Background()
	run, _ := rg.rt.store.CreateRun(ctx, sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "danger", map[string]any{})
	ap, err := rg.rt.store.CreateApproval(ctx, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "danger", Kind: KindToolApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rg.Decide(ctx, ap.ID, true, nil, nil, nil); !errors.Is(err, ErrNoSuspendedBatch) {
		t.Fatalf("Decide = %v, want ErrNoSuspendedBatch", err)
	}
}

// TestFoldBatchArgsErrorNeverExecutes: a sibling whose arguments never parsed
// (the loop refused it, no interaction row) must not execute at fold time
// either — the durable ArgsError marker folds it as an is_error result, while
// its gated sibling still executes per the verdict.
func TestFoldBatchArgsErrorNeverExecutes(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ctx := context.Background()
	run, _ := rg.rt.store.CreateRun(ctx, sess.ID, 1)
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "turn"}},
	})
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "tu_g", ToolName: "danger", ToolInput: map[string]any{}},
			// The malformed-args sibling: ToolInput nil (parse failed), the
			// parse failure persisted on the block by appendFinalized.
			{Type: provider.BlockToolUse, ToolUseID: "tu_bad", ToolName: "write_file", ArgsError: "tool arguments are not valid JSON: unexpected end"},
		},
	})
	ap := createSuspendedInteraction(t, rg, []string{"tu_g", "tu_bad"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu_g", ToolName: "danger", Kind: KindToolApproval,
	})

	dangerRuns, writeRuns := 0, 0
	reg := toolruntime.NewRegistry()
	reg.Register(countTool{name: "danger", runs: &dangerRuns})
	reg.Register(countTool{name: "write_file", runs: &writeRuns})

	_, history, err := rg.Decide(ctx, ap.ID, true, nil, reg, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dangerRuns != 1 {
		t.Errorf("danger executed %d times, want 1 (the approved gated call)", dangerRuns)
	}
	if writeRuns != 0 {
		t.Errorf("write_file executed %d times, want 0 — a malformed-args call must never execute at fold", writeRuns)
	}
	last := history[len(history)-1]
	if len(last.Content) != 2 {
		t.Fatalf("folded results = %+v, want [tu_g tu_bad]", last.Content)
	}
	if !last.Content[1].IsError || !strings.Contains(last.Content[1].ToolContent, "invalid tool arguments") {
		t.Errorf("tu_bad result = %+v, want an is_error 'invalid tool arguments' result", last.Content[1])
	}
	if last.Content[0].IsError {
		t.Errorf("tu_g result = %+v, want success (approved)", last.Content[0])
	}
}

// TestFoldBatchHardDeniedSiblingNeverExecutes: a call the policy HARD-denies
// (deny without the approval marker) never becomes an interaction — the
// interaction gate suspends only on approval-marker denies — so in a mixed
// batch it is an un-gated sibling. The loop's dispatch screen would have
// refused it; the fold must re-apply the execution gate and refuse it too,
// or one policy's outcome would depend on whether the batch happened to
// contain an approval-gated neighbour.
func TestFoldBatchHardDeniedSiblingNeverExecutes(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ctx := context.Background()
	run, _ := rg.rt.store.CreateRun(ctx, sess.ID, 1)
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "turn"}},
	})
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "tu_g", ToolName: "danger", ToolInput: map[string]any{}},
			// Hard-denied sibling (e.g. PERMISSION_NETWORK=deny): no
			// interaction row exists for it.
			{Type: provider.BlockToolUse, ToolUseID: "tu_d", ToolName: "network_fetch", ToolInput: map[string]any{"url": "x"}},
		},
	})
	ap := createSuspendedInteraction(t, rg, []string{"tu_g", "tu_d"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu_g", ToolName: "danger", Kind: KindToolApproval,
	})

	dangerRuns, fetchRuns := 0, 0
	reg := toolruntime.NewRegistry()
	reg.Register(countTool{name: "danger", runs: &dangerRuns})
	reg.Register(countTool{name: "network_fetch", runs: &fetchRuns})
	// The loop's execution-gate policy, threaded into the fold: network_fetch
	// is hard-denied (no approval marker — a verdict cannot lift it).
	gate := func(_ context.Context, tool toolruntime.Tool) (bool, string) {
		if tool.Name() == "network_fetch" {
			return true, "network_fetch (risk: network) is not permitted by policy"
		}
		return false, ""
	}

	_, history, err := rg.Decide(ctx, ap.ID, true, nil, reg, gate)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dangerRuns != 1 {
		t.Errorf("danger executed %d times, want 1 (the approved gated call)", dangerRuns)
	}
	if fetchRuns != 0 {
		t.Errorf("network_fetch executed %d times, want 0 — a hard-denied call must not execute at fold", fetchRuns)
	}
	last := history[len(history)-1]
	if len(last.Content) != 2 {
		t.Fatalf("folded results = %+v, want [tu_g tu_d]", last.Content)
	}
	if !last.Content[1].IsError || !strings.Contains(last.Content[1].ToolContent, "permission denied") {
		t.Errorf("tu_d result = %+v, want an is_error 'permission denied' result", last.Content[1])
	}
	if last.Content[0].IsError {
		t.Errorf("tu_g result = %+v, want success (approved)", last.Content[0])
	}

	// The hard-deny is folded, not executed: a resume retry stays idempotent.
	if _, err := rg.FoldBatch(ctx, sess.ID, run.ID, reg, gate); err != nil {
		t.Fatalf("FoldBatch retry: %v", err)
	}
	if fetchRuns != 0 {
		t.Error("retry executed the hard-denied call")
	}
}

// ctxSensitiveTool fails when its context is cancelled, so a test can prove
// the fold executed it on a live (detached) context.
type ctxSensitiveTool struct {
	name string
	runs *int
}

func (c ctxSensitiveTool) Name() string           { return c.name }
func (c ctxSensitiveTool) Description() string    { return "ctx-sensitive" }
func (c ctxSensitiveTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (c ctxSensitiveTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }
func (c ctxSensitiveTool) Timeout() time.Duration { return time.Second }
func (c ctxSensitiveTool) Call(ctx context.Context, _ map[string]any) (toolruntime.Result, error) {
	if err := ctx.Err(); err != nil {
		return toolruntime.Result{Content: "cancelled mid-fold", IsError: true}, nil
	}
	*c.runs++
	return toolruntime.Result{Content: c.name + " done"}, nil
}

// TestFoldBatchCompletesDespiteCancelledCaller: the fold is the batch's
// durable completion, not request-scoped work — a client that disconnects
// right after POSTing the final verdict (cancelling the request ctx) must not
// abort tool execution or the commit. The decision already committed and a
// decided row renders no pending card, so nothing would ever retry.
func TestFoldBatchCompletesDespiteCancelledCaller(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ctx := context.Background()
	run, _ := rg.rt.store.CreateRun(ctx, sess.ID, 1)
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "turn"}},
	})
	_, _ = ms.AppendMessage(ctx, StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "tu_g", ToolName: "danger", ToolInput: map[string]any{}},
			{Type: provider.BlockToolUse, ToolUseID: "tu_p", ToolName: "load_skill", ToolInput: map[string]any{}},
		},
	})
	ap := createSuspendedInteraction(t, rg, []string{"tu_g", "tu_p"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu_g", ToolName: "danger", Kind: KindToolApproval,
	})
	if _, _, err := rg.RecordDecision(ctx, ap.ID, true, nil); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	dangerRuns, readRuns := 0, 0
	reg := toolruntime.NewRegistry()
	reg.Register(ctxSensitiveTool{name: "danger", runs: &dangerRuns})
	reg.Register(ctxSensitiveTool{name: "load_skill", runs: &readRuns})

	// The "client disconnected" fold: every caller-facing ctx is already
	// cancelled, yet the fold must run to completion and persist.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	history, err := rg.FoldBatch(cancelled, sess.ID, run.ID, reg, nil)
	if err != nil {
		t.Fatalf("FoldBatch with a cancelled caller ctx: %v", err)
	}
	if dangerRuns != 1 || readRuns != 1 {
		t.Errorf("runs = danger:%d load_skill:%d, want 1 each — tools must see a live, detached ctx", dangerRuns, readRuns)
	}
	if len(history) == 0 {
		t.Error("history empty — the fold must still rebuild and return it")
	}
	folded, _, err := rg.BatchFoldState(ctx, run.ID)
	if err != nil || !folded {
		t.Errorf("batch folded = %v, err %v — the commit must land despite the cancelled caller", folded, err)
	}
}

// TestFoldBatchIdempotentRetry: a retried resume after a committed fold
// re-executes nothing and persists no duplicate.
func TestFoldBatchIdempotentRetry(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	ctx := context.Background()
	run, _ := rg.rt.store.CreateRun(ctx, sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "danger", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "danger", Kind: KindToolApproval,
	})

	dangerRuns := 0
	reg := toolruntime.NewRegistry()
	reg.Register(countTool{name: "danger", runs: &dangerRuns})

	if _, _, err := rg.Decide(ctx, ap.ID, true, nil, reg, nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dangerRuns != 1 {
		t.Fatalf("danger executed %d times, want 1", dangerRuns)
	}
	afterFold, _ := ms.MessagesFor(ctx, sess.ID)

	// Retry the resume (client timeout / crash between fold commit and response).
	history, err := rg.FoldBatch(ctx, sess.ID, run.ID, reg, nil)
	if err != nil {
		t.Fatalf("FoldBatch retry: %v", err)
	}
	if dangerRuns != 1 {
		t.Errorf("retry re-executed the tool (runs = %d), want idempotent", dangerRuns)
	}
	afterRetry, _ := ms.MessagesFor(ctx, sess.ID)
	if len(afterRetry) != len(afterFold) {
		t.Errorf("retry persisted a duplicate (messages %d → %d)", len(afterFold), len(afterRetry))
	}
	if len(history) == 0 || history[len(history)-1].Content[0].ToolResultID != "tu1" {
		t.Errorf("retry history = %+v, want it to end with the folded result", history)
	}
}
