package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// twoGatedProvider drives one assistant turn carrying TWO permission-gated tool
// calls (tu1, tu2), so the loop surfaces a batch of two interrupts and ends.
type twoGatedProvider struct{}

func (p *twoGatedProvider) Name() string { return "two-gated" }

func (p *twoGatedProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 12)
	mk := func(idx int, id, name string) {
		ch <- provider.Event{Type: provider.EventBlockStart, Index: idx, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: idx, Delta: `{}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: idx}
	}
	ch <- provider.Event{Type: provider.EventMessageStart}
	mk(0, "tu1", "edit_a")
	mk(1, "tu2", "edit_b")
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopToolUse}
	close(ch)
	return ch, nil
}

type gatedTool struct{ name string }

func (g gatedTool) Name() string           { return g.name }
func (g gatedTool) Description() string    { return "gated" }
func (g gatedTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (g gatedTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }
func (g gatedTool) Timeout() time.Duration { return time.Second }
func (g gatedTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{Content: "edited " + g.name}, nil
}

// TestRegistryBatchDecisionFlow pins the multi-approval queue end to end at the
// registry seam: a two-call gated batch parks TWO pending interactions; the
// first RecordDecision reports batchComplete=false (no resume, no fold); the
// second reports complete, and FoldBatch folds BOTH calls' results into one user
// message — leaving no dangling tool_use for EnsurePairing to synthesize.
func TestRegistryBatchDecisionFlow(t *testing.T) {
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

	// Two pending interactions (one per gated call), in queue order.
	queue, err := rg.PendingApprovalsForSession(context.Background(), sess.ID)
	if err != nil || len(queue) != 2 {
		t.Fatalf("pending queue = %+v err %v, want 2", queue, err)
	}
	if queue[0].ToolCallID != "tu1" || queue[1].ToolCallID != "tu2" {
		t.Errorf("queue order = %s,%s want tu1,tu2", queue[0].ToolCallID, queue[1].ToolCallID)
	}

	// First verdict: batch incomplete — no fold, no resume signal.
	_, complete, err := rg.RecordDecision(context.Background(), queue[0].ID, true, nil)
	if err != nil {
		t.Fatalf("RecordDecision 1: %v", err)
	}
	if complete {
		t.Fatal("batch should NOT be complete after the first of two verdicts")
	}

	// Second verdict: batch complete.
	_, complete, err = rg.RecordDecision(context.Background(), queue[1].ID, false, nil)
	if err != nil {
		t.Fatalf("RecordDecision 2: %v", err)
	}
	if !complete {
		t.Fatal("batch should be complete after the second verdict")
	}

	// Fold the whole batch: one user message with BOTH tool_results (approve → the
	// executed result; reject → an is_error denial), matching the two tool_uses.
	history, err := rg.FoldBatch(context.Background(), sess.ID, run.ID, loop.Tools(), nil)
	if err != nil {
		t.Fatalf("FoldBatch: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("FoldBatch returned empty history")
	}
	last := history[len(history)-1]
	if last.Role != provider.RoleUser {
		t.Fatalf("folded message role = %q, want user", last.Role)
	}
	var results []provider.Block
	for _, b := range last.Content {
		if b.Type == provider.BlockToolResult {
			results = append(results, b)
		}
	}
	if len(results) != 2 {
		t.Fatalf("folded %d tool_results, want 2", len(results))
	}
	if results[0].ToolResultID != "tu1" || results[0].IsError {
		t.Errorf("tu1 result = %+v, want approved non-error", results[0])
	}
	if results[1].ToolResultID != "tu2" || !results[1].IsError {
		t.Errorf("tu2 result = %+v, want rejected is_error", results[1])
	}

	// EnsurePairing over the folded history must synthesize NOTHING: every tool_use
	// has a matching tool_result (the whole point of waiting for the full batch).
	paired := contextmgmt.EnsurePairing(history)
	for _, m := range paired {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolContent == "[Tool use interrupted]" {
				t.Errorf("EnsurePairing synthesized an interrupted result for %s — batch was not complete", b.ToolResultID)
			}
		}
	}
}

// failBatchStore wraps MemStore and fails CreateInteractionBatch once it has
// succeeded failAfter times — a durable write dying mid-way through a gated
// batch's interrupt frames (earlier rows already persisted).
type failBatchStore struct {
	*MemStore
	failAfter int
	calls     int
}

func (s *failBatchStore) CreateInteractionBatch(ctx context.Context, batch SuspendedBatch, in Interaction) (Interaction, error) {
	s.calls++
	if s.calls > s.failAfter {
		return Interaction{}, errors.New("interaction store down")
	}
	return s.MemStore.CreateInteractionBatch(ctx, batch, in)
}

// TestRegistryVoidsLeftoverInteractionsOnFailedRun pins the deadlock fix: when
// the second interrupt frame of a gated batch fails to persist, the run fails —
// but the FIRST row is already durable. The worker must void that leftover, or
// the session's pending-interaction gate would reject every later submission
// (a permanent 409 no client can resolve).
func TestRegistryVoidsLeftoverInteractionsOnFailedRun(t *testing.T) {
	store := &failBatchStore{MemStore: NewMemStore(), failAfter: 1}
	rt := NewRuntime(store).WithBus(NewMemBus())
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
	if got := waitSettle(t, rt, sess.ID); got != RunFailed {
		t.Fatalf("run status = %v, want failed (the second interrupt write failed)", got)
	}

	// The leftover row (persisted before the failure) was voided: nothing stays
	// pending for the session, so the pending-interaction gate clears.
	pending, err := rg.PendingApprovalsForSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending queue = %+v, want empty — a leftover row would 409 every later submission", pending)
	}
	// And it is resolved (rejected by the void), not deleted: the audit trail
	// of the attempted batch survives.
	all, err := store.ApprovalsForRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != InteractionRejected {
		t.Errorf("run interactions = %+v, want the one persisted row rejected", all)
	}
}
