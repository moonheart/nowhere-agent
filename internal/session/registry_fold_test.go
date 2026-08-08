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

	_, history, err := rg.Decide(ctx, ap.ID, true, nil, reg)
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
	_, _, err := rg.Decide(ctx, ap.ID, true, nil, reg)
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
	if _, _, err := rg.Decide(ctx, ap.ID, true, nil, nil); !errors.Is(err, ErrNoSuspendedBatch) {
		t.Fatalf("Decide = %v, want ErrNoSuspendedBatch", err)
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

	if _, _, err := rg.Decide(ctx, ap.ID, true, nil, reg); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dangerRuns != 1 {
		t.Fatalf("danger executed %d times, want 1", dangerRuns)
	}
	afterFold, _ := ms.MessagesFor(ctx, sess.ID)

	// Retry the resume (client timeout / crash between fold commit and response).
	history, err := rg.FoldBatch(ctx, sess.ID, run.ID, reg)
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
