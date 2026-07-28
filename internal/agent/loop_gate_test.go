package agent

import (
	"context"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// askAll gates every tool call for human approval (the Ask marker).
func askAll(toolruntime.Tool) (bool, string) {
	return true, ApprovalReasonPrefix + "ask"
}

// TestLoopEndsOnApprovalGate pins the run-stateless gate: a permission-gated
// tool call is NOT executed; the loop sets PendingApproval, emits
// KindApprovalRequest, and ends the run cleanly (nil error, no further provider
// call). The assistant message carries the gated tool_use for the worker to
// persist; a LATER run applies the verdict.
func TestLoopEndsOnApprovalGate(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "danger", `{"path":"/etc"}`),
		// No second script entry: the loop must end after the gate, not continue.
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(riskTool{name: "danger", risk: toolruntime.RiskExternalWrite, called: &called})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})
	loop.Use(&PermissionMW{Check: askAll})

	emit := &memEmitter{}
	produced, err := loop.Run(context.Background(), nil, emit)
	if err != nil {
		t.Fatalf("gate should end the run cleanly, got err %v", err)
	}
	if called {
		t.Error("gated tool must not execute")
	}
	if loop.PendingApproval == nil {
		t.Fatal("PendingApproval not set after gate")
	}
	if loop.PendingApproval.ID == "" {
		t.Error("PendingApproval.ID not generated; the frame must carry the approval id")
	}
	if loop.PendingApproval.ToolCallID != "tu1" || loop.PendingApproval.ToolName != "danger" || loop.PendingApproval.Kind != "approval" {
		t.Errorf("PendingApproval = %+v", loop.PendingApproval)
	}
	// The assistant message (with the gated tool_use) was produced for persistence.
	if len(produced) != 1 || produced[0].Content[0].Type != provider.BlockToolUse {
		t.Fatalf("produced = %+v, want one assistant msg with the tool_use", produced)
	}
	// A KindApprovalRequest frame was emitted (drives the client card).
	if emit.count(KindApprovalRequest) == 0 {
		t.Errorf("no KindApprovalRequest emitted; events=%v", emit.events)
	}
}

// TestLoopEndsOnAskUser pins the ask_user gate: calling ask_user ends the run
// (no permission needed) with Kind=ask_user.
func TestLoopEndsOnAskUser(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", AskUserToolName, `{"questions":[]}`),
	}}
	reg := toolruntime.NewRegistry()
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatalf("ask_user gate should end cleanly, got %v", err)
	}
	if loop.PendingApproval == nil || loop.PendingApproval.Kind != "ask_user" {
		t.Fatalf("PendingApproval = %+v, want ask_user", loop.PendingApproval)
	}
	if loop.PendingApproval.ID == "" {
		t.Error("PendingApproval.ID not generated for ask_user")
	}
	if len(produced) != 1 || produced[0].Content[0].Type != provider.BlockToolUse {
		t.Fatalf("produced = %+v", produced)
	}
}
