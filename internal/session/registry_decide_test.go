package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// decideTool records execution for the approved-tool path.
type decideTool struct{ ran *bool }

func (d decideTool) Name() string           { return "danger" }
func (d decideTool) Description() string    { return "gated" }
func (d decideTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (d decideTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }
func (d decideTool) Timeout() time.Duration { return time.Second }
func (d decideTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	*d.ran = true
	return toolruntime.Result{Content: "danger done"}, nil
}

// seedGatedConversation persists a user turn + an assistant turn carrying a
// gated tool_use (no tool_result yet) — the state a run leaves behind when it
// ends on an approval gate.
func seedGatedConversation(t *testing.T, rg *RunRegistry, ms MessageStore, sessionID, runID, toolCallID, toolName string, toolInput map[string]any) {
	t.Helper()
	_, _ = ms.AppendMessage(context.Background(), StoredMessage{
		SessionID: sessionID, RunID: runID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "do it"}},
	})
	_, _ = ms.AppendMessage(context.Background(), StoredMessage{
		SessionID: sessionID, RunID: runID, Role: provider.RoleAssistant,
		Content: []provider.Block{{
			Type: provider.BlockToolUse, ToolUseID: toolCallID, ToolName: toolName,
			ToolInput: toolInput,
		}},
	})
}

// createSuspendedInteraction persists a pending interaction together with the
// suspended-batch snapshot the fold path requires (capability
// suspend-batch-snapshot). batchIDs is the full batch in tool_use order.
func createSuspendedInteraction(t *testing.T, rg *RunRegistry, batchIDs []string, in Interaction) Interaction {
	t.Helper()
	ap, err := rg.rt.store.CreateInteractionBatch(context.Background(), SuspendedBatch{
		RunID: in.RunID, SessionID: in.SessionID, ToolCallIDs: batchIDs,
	}, in)
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

func newDecideRegistry(t *testing.T) (*RunRegistry, MessageStore, Session) {
	t.Helper()
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	rg := NewRunRegistry(rt)
	ms := NewMemMessageStore()
	rg.WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}
	return rg, ms, sess
}

// TestDecideApprovedExecutesTool pins the run-stateless verdict path: an
// approved permission call is EXECUTED during Decide, and the returned history
// ends with the tool_use's tool_result so a fresh run can continue.
func TestDecideApprovedExecutesTool(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "danger", map[string]any{"path": "/etc"})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Approval{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "danger",
		Payload: json.RawMessage(`{"path":"/etc"}`), Kind: "approval",
	})

	ran := false
	reg := toolruntime.NewRegistry()
	reg.Register(decideTool{ran: &ran})

	got, history, err := rg.Decide(context.Background(), ap.ID, true, nil, reg, nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !ran {
		t.Error("approved tool was not executed")
	}
	if got.Status != ApprovalApproved {
		t.Errorf("approval status = %v want approved", got.Status)
	}
	// History = user, assistant(tool_use), tool_result.
	if len(history) != 3 {
		t.Fatalf("history len = %d want 3: %+v", len(history), history)
	}
	last := history[len(history)-1]
	if last.Role != provider.RoleUser || len(last.Content) != 1 || last.Content[0].Type != provider.BlockToolResult {
		t.Fatalf("last message not a tool_result: %+v", last)
	}
	tr := last.Content[0]
	if tr.ToolResultID != "tu1" || tr.ToolContent != "danger done" || tr.IsError {
		t.Errorf("tool_result = %+v want {tu1, danger done, no error}", tr)
	}
}

// TestDecideRejectedInjectsDenial: a rejected permission call is NOT executed;
// history ends with an is_error denial.
func TestDecideRejectedInjectsDenial(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "danger", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Approval{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "danger", Kind: "approval",
	})

	ran := false
	reg := toolruntime.NewRegistry()
	reg.Register(decideTool{ran: &ran})

	_, history, err := rg.Decide(context.Background(), ap.ID, false, nil, reg, nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if ran {
		t.Error("rejected tool must not execute")
	}
	tr := history[len(history)-1].Content[0]
	if !tr.IsError {
		t.Errorf("denial should be is_error, got %+v", tr)
	}
}

// TestDecideAskUserAnswer: an ask_user answer becomes the (non-error) tool
// result; a skip (approve=false) becomes a "skipped" note.
func TestDecideAskUserAnswer(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "ask_user", map[string]any{"questions": []any{}})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Approval{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "ask_user", Kind: "ask_user",
	})

	answer := json.RawMessage(`{"answers":{"q":"x"}}`)
	_, history, err := rg.Decide(context.Background(), ap.ID, true, answer, nil, nil, nil)
	if err != nil {
		t.Fatalf("Decide answer: %v", err)
	}
	tr := history[len(history)-1].Content[0]
	if tr.IsError || tr.ToolContent != `{"answers":{"q":"x"}}` {
		t.Errorf("ask_user answer tool_result = %+v", tr)
	}
}

func TestDecideAskUserSkipped(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "ask_user", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Approval{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "ask_user", Kind: "ask_user",
	})

	_, history, err := rg.Decide(context.Background(), ap.ID, false, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Decide skip: %v", err)
	}
	tr := history[len(history)-1].Content[0]
	if tr.IsError || tr.ToolContent == "" {
		t.Errorf("skip tool_result should be a non-error note, got %+v", tr)
	}
}

// TestDecideUnknownOrDecided: an unknown or already-decided approval errors.
func TestDecideUnknownOrDecided(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)
	seedGatedConversation(t, rg, ms, sess.ID, run.ID, "tu1", "danger", map[string]any{})
	ap := createSuspendedInteraction(t, rg, []string{"tu1"}, Approval{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "danger", Kind: "approval",
	})
	if _, _, err := rg.Decide(context.Background(), "no-such", true, nil, nil, nil, nil); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("unknown decide: %v", err)
	}
	if _, _, err := rg.Decide(context.Background(), ap.ID, false, nil, nil, nil, nil); err != nil {
		t.Fatalf("first decide: %v", err)
	}
	if _, _, err := rg.Decide(context.Background(), ap.ID, true, nil, nil, nil, nil); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("second decide should error, got %v", err)
	}
}

// readTool records execution for the un-gated sibling in a parallel batch.
type readTool struct{ ran *bool }

func (d readTool) Name() string           { return "load_skill" }
func (d readTool) Description() string    { return "read-only" }
func (d readTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (d readTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (d readTool) Timeout() time.Duration { return time.Second }
func (d readTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	*d.ran = true
	return toolruntime.Result{Content: "skill body"}, nil
}

// TestDecideParallelBatchResumesUngatedCall pins the resume bug fix: a batch of
// parallel tool_use calls where ONE gated (approval) suspended the whole run
// before dispatch, so its un-gated sibling never executed and had no tool_result.
// On resume, the gated call folds its verdict AND the un-gated sibling is
// dispatched now — both get a tool_result, so the model sees no dangling tool_use.
func TestDecideParallelBatchResumesUngatedCall(t *testing.T) {
	rg, ms, sess := newDecideRegistry(t)
	run, _ := rg.rt.store.CreateRun(context.Background(), sess.ID, 1)

	// The suspended assistant message carries TWO tool_use blocks in one batch:
	// tu1 = load_skill (read-only, never gated, never ran), tu2 = danger (gated).
	_, _ = ms.AppendMessage(context.Background(), StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "load then run"}},
	})
	_, _ = ms.AppendMessage(context.Background(), StoredMessage{
		SessionID: sess.ID, RunID: run.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "load_skill", ToolInput: map[string]any{"name": "calc"}},
			{Type: provider.BlockToolUse, ToolUseID: "tu2", ToolName: "danger", ToolInput: map[string]any{"path": "/etc"}},
		},
	})
	ap := createSuspendedInteraction(t, rg, []string{"tu1", "tu2"}, Approval{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu2", ToolName: "danger",
		Payload: json.RawMessage(`{"path":"/etc"}`), Kind: "approval",
	})

	readRan, dangerRan := false, false
	reg := toolruntime.NewRegistry()
	reg.Register(readTool{ran: &readRan})
	reg.Register(decideTool{ran: &dangerRan})

	_, history, err := rg.Decide(context.Background(), ap.ID, true, nil, reg, nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !dangerRan {
		t.Error("approved gated tool was not executed")
	}
	if !readRan {
		t.Error("un-gated sibling was not dispatched on resume (the dropped-call bug)")
	}

	// History = user, assistant(tool_use×2), ONE folded tool_result message with
	// BOTH results in batch order (tu1 first, then tu2).
	if len(history) != 3 {
		t.Fatalf("history len = %d want 3: %+v", len(history), history)
	}
	last := history[len(history)-1]
	if last.Role != provider.RoleUser || len(last.Content) != 2 {
		t.Fatalf("folded message should carry 2 tool_results, got %+v", last)
	}
	if last.Content[0].ToolResultID != "tu1" || last.Content[0].ToolContent != "skill body" || last.Content[0].IsError {
		t.Errorf("tool_result[0] = %+v want {tu1, skill body, no error}", last.Content[0])
	}
	if last.Content[1].ToolResultID != "tu2" || last.Content[1].ToolContent != "danger done" || last.Content[1].IsError {
		t.Errorf("tool_result[1] = %+v want {tu2, danger done, no error}", last.Content[1])
	}

	// The folded message is persisted too, so the NEXT resume sees both answers.
	stored, _ := ms.MessagesFor(context.Background(), sess.ID)
	folded := stored[len(stored)-1]
	if len(folded.Content) != 2 || folded.Content[0].ToolResultID != "tu1" || folded.Content[1].ToolResultID != "tu2" {
		t.Errorf("persisted folded message = %+v want both tool_results", folded)
	}
}
