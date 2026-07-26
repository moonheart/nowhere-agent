package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// gatedTool is a tool the permission callback gates for approval.
type gatedTool struct{ called *bool }

func (g gatedTool) Name() string           { return "danger" }
func (g gatedTool) Description() string    { return "a dangerous tool" }
func (g gatedTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (g gatedTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }
func (g gatedTool) Timeout() time.Duration { return time.Second }
func (g gatedTool) Call(_ context.Context, args map[string]any) (toolruntime.Result, error) {
	*g.called = true
	return toolruntime.Result{Content: "did the dangerous thing"}, nil
}

// gateAll is a Permission that marks every call as approval-required.
func gateAll(toolruntime.Tool) (bool, string) {
	return true, ApprovalReasonPrefix + "policy says ask"
}

// TestLoopSuspendsOnApproval pins O2: a permission-gated tool call is NOT
// executed; the loop emits KindApprovalRequest, records the PendingApproval,
// and returns ErrAwaitingApproval instead of finishing or erroring.
func TestLoopSuspendsOnApproval(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "danger", `{"path":"/etc"}`),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(gatedTool{called: &called})
	emit := &memEmitter{}
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100, Permission: gateAll})

	_, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "do it")}, emit)
	if !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("err = %v, want ErrAwaitingApproval", err)
	}
	if called {
		t.Error("gated tool must NOT execute before approval")
	}
	if loop.PendingApproval == nil {
		t.Fatal("PendingApproval not recorded")
	}
	if loop.PendingApproval.ToolCallID != "tu1" || loop.PendingApproval.ToolName != "danger" {
		t.Errorf("PendingApproval = %+v", loop.PendingApproval)
	}
	if emit.count(KindApprovalRequest) != 1 {
		t.Errorf("expected 1 approval_request event, got %d", emit.count(KindApprovalRequest))
	}
	if emit.count(KindToolResult) != 0 {
		t.Error("no tool result should be emitted for a suspended call")
	}
}

// TestLoopResumeApprovedExecutes: a resumed run with Approval.Approved=true
// executes the gated call (without re-checking Permission) and continues.
func TestLoopResumeApprovedExecutes(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		textResponse("done after approval"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(gatedTool{called: &called})
	emit := &memEmitter{}
	loop := New(p, reg, Config{
		Model: "m", MaxTokens: 100,
		Permission: gateAll, // even though gated, the approval authorizes the call
		Approval:   &ResumedApproval{ToolCallID: "tu1", ToolName: "danger", Input: map[string]any{"path": "/etc"}, Approved: true},
	})

	produced, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "do it")}, emit)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if !called {
		t.Error("approved call should have executed")
	}
	// First produced message is the approval tool_result; then the final answer.
	if len(produced) != 2 {
		t.Fatalf("expected approval-result + final, got %d: %+v", len(produced), produced)
	}
	if produced[0].Content[0].Type != provider.BlockToolResult || produced[0].Content[0].ToolResultID != "tu1" {
		t.Errorf("msg0 should be the approval tool_result, got %+v", produced[0])
	}
	if produced[0].Content[0].ToolContent != "did the dangerous thing" {
		t.Errorf("approval result = %q", produced[0].Content[0].ToolContent)
	}
	if emit.count(KindDone) != 1 {
		t.Error("expected run to complete after approval")
	}
}

// TestLoopResumeRejectedInjectsDenial: a resumed run with Approval.Approved=false
// does NOT execute; it feeds an is_error denial back so the model adapts.
func TestLoopResumeRejectedInjectsDenial(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		textResponse("understood, I won't"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(gatedTool{called: &called})
	emit := &memEmitter{}
	loop := New(p, reg, Config{
		Model: "m", MaxTokens: 100,
		Approval: &ResumedApproval{ToolCallID: "tu1", ToolName: "danger", Input: map[string]any{}, Approved: false},
	})

	produced, err := loop.Run(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "do it")}, emit)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if called {
		t.Error("rejected call must NOT execute")
	}
	if len(produced) != 2 || produced[0].Content[0].Type != provider.BlockToolResult {
		t.Fatalf("expected denial tool_result + final, got %+v", produced)
	}
	if !produced[0].Content[0].IsError {
		t.Error("rejected result should be is_error")
	}
}

// TestIsApprovalReason pins the marker contract the server relies on.
func TestIsApprovalReason(t *testing.T) {
	if !IsApprovalReason(ApprovalReasonPrefix + "ask the user") {
		t.Error("prefixed reason should be an approval gate")
	}
	if IsApprovalReason("hard deny") {
		t.Error("plain deny should not be an approval gate")
	}
}
