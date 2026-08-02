package agent

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// askAll gates every tool call for human approval (the Ask marker).
func askAll(context.Context, toolruntime.Tool) (bool, string) {
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

// TestLoopGatedBatchEmitsEveryInterrupt pins the multi-approval queue: when one
// turn emits several permission-gated calls, the loop surfaces EVERY one as its
// own interrupt (not just the first), sets the full PendingInteractions batch,
// executes none of them, and ends the run. A fresh run resumes only once the
// whole batch is resolved, so the model never sees a half-decided turn.
func TestLoopGatedBatchEmitsEveryInterrupt(t *testing.T) {
	calls := map[string]bool{}
	called := func(name string) *bool {
		b := false
		calls[name] = b
		return &b
	}
	// One assistant message carrying THREE gated tool calls in one turn.
	turn := []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "edit1", ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: `{}`},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventBlockStart, Index: 1, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu2", ToolName: "edit2", ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 1, Delta: `{}`},
		{Type: provider.EventBlockStop, Index: 1},
		{Type: provider.EventBlockStart, Index: 2, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu3", ToolName: "edit3", ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 2, Delta: `{}`},
		{Type: provider.EventBlockStop, Index: 2},
		{Type: provider.EventMessageStop},
	}
	p := &scriptProvider{script: [][]provider.Event{turn}}
	reg := toolruntime.NewRegistry()
	reg.Register(riskTool{name: "edit1", risk: toolruntime.RiskExternalWrite, called: called("edit1")})
	reg.Register(riskTool{name: "edit2", risk: toolruntime.RiskExternalWrite, called: called("edit2")})
	reg.Register(riskTool{name: "edit3", risk: toolruntime.RiskExternalWrite, called: called("edit3")})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})
	loop.Use(&PermissionMW{Check: askAll})

	emit := &memEmitter{}
	produced, err := loop.Run(context.Background(), nil, emit)
	if err != nil {
		t.Fatalf("gated batch should end the run cleanly, got %v", err)
	}
	// None of the gated calls executed.
	for name, b := range calls {
		if b {
			t.Errorf("gated tool %s executed", name)
		}
	}
	// The whole batch is pending, in tool_use order; the head aliases the singulars.
	if len(loop.PendingInteractions) != 3 {
		t.Fatalf("PendingInteractions = %d, want 3", len(loop.PendingInteractions))
	}
	for i, id := range []string{"tu1", "tu2", "tu3"} {
		if loop.PendingInteractions[i].ToolCallID != id {
			t.Errorf("PendingInteractions[%d].ToolCallID = %s, want %s", i, loop.PendingInteractions[i].ToolCallID, id)
		}
		if loop.PendingInteractions[i].ID == "" {
			t.Errorf("PendingInteractions[%d].ID not generated", i)
		}
	}
	if loop.PendingInteraction != loop.PendingInteractions[0] || loop.PendingApproval != loop.PendingInteractions[0] {
		t.Error("singular PendingInteraction/PendingApproval should alias the batch head")
	}
	// Every gated call got its own interrupt frame.
	if n := emit.count(KindInterrupt); n != 3 {
		t.Errorf("KindInterrupt count = %d, want 3; events=%v", n, emit.events)
	}
	// The assistant message carries all three gated tool_use blocks for persistence.
	if len(produced) != 1 {
		t.Fatalf("produced = %d messages, want 1 assistant turn", len(produced))
	}
	var uses int
	for _, b := range produced[0].Content {
		if b.Type == provider.BlockToolUse {
			uses++
		}
	}
	if uses != 3 {
		t.Errorf("assistant message has %d tool_use blocks, want 3", uses)
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
