package agent

import (
	"context"
	"testing"
	"time"

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

// TestLoopEndsOnClientTool pins the client-side-tool gate: a tool implementing
// toolruntime.ClientTool suspends the run (no permission needed, no server
// execution) with Kind=client_tool. The loop emits KindInterrupt and ends
// cleanly; the tool's Call is never reached.
func TestLoopEndsOnClientTool(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "get_clipboard", `{}`),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(clientSideTool{name: "get_clipboard", called: &called})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	emit := &memEmitter{}
	produced, err := loop.Run(context.Background(), nil, emit)
	if err != nil {
		t.Fatalf("client-tool gate should end cleanly, got %v", err)
	}
	if called {
		t.Error("a client-side tool must not execute on the server")
	}
	if loop.PendingInteraction == nil || loop.PendingInteraction.Kind != "client_tool" {
		t.Fatalf("PendingInteraction = %+v, want client_tool", loop.PendingInteraction)
	}
	if loop.PendingInteraction.ID == "" {
		t.Error("PendingInteraction.ID not generated for client_tool")
	}
	// PendingApproval aliases the same value (source compatibility).
	if loop.PendingApproval != loop.PendingInteraction {
		t.Error("PendingApproval should alias PendingInteraction")
	}
	if len(produced) != 1 || produced[0].Content[0].Type != provider.BlockToolUse {
		t.Fatalf("produced = %+v", produced)
	}
	if emit.count(KindInterrupt) == 0 {
		t.Errorf("no KindInterrupt emitted; events=%v", emit.events)
	}
}

// clientSideTool is a toolruntime.ClientTool: it declares ClientSide()=true and
// an output schema; Call records execution (which must never happen in the gated
// path).
type clientSideTool struct {
	name   string
	called *bool
}

func (c clientSideTool) Name() string           { return c.name }
func (c clientSideTool) Description() string    { return "client tool" }
func (c clientSideTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (c clientSideTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (c clientSideTool) Timeout() time.Duration { return 0 }
func (c clientSideTool) ClientSide() bool       { return true }
func (c clientSideTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
}
func (c clientSideTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	if c.called != nil {
		*c.called = true
	}
	return toolruntime.Result{Content: "server-ran"}, nil
}
