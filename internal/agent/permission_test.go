package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// riskTool records whether it was executed, so a test can prove a denied call
// never runs.
type riskTool struct {
	name   string
	risk   toolruntime.Risk
	called *bool
}

func (r riskTool) Name() string             { return r.name }
func (r riskTool) Description() string       { return "risk tool" }
func (r riskTool) Schema() map[string]any    { return map[string]any{"type": "object"} }
func (r riskTool) Risk() toolruntime.Risk    { return r.risk }
func (r riskTool) Timeout() time.Duration    { return time.Second }
func (r riskTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	if r.called != nil {
		*r.called = true
	}
	return toolruntime.Result{Content: "did the thing"}, nil
}

// denyNetwork denies any network-risk tool.
func denyNetwork(t toolruntime.Tool) (bool, string) {
	if t.Risk() == toolruntime.RiskNetwork {
		return true, "network is not permitted by policy"
	}
	return false, ""
}

// TestLoopPermissionDeniesGatedTool verifies a denied tool is not executed and
// the model receives an is_error result so it can adapt, and the loop continues.
func TestLoopPermissionDeniesGatedTool(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "net", `{}`),
		textResponse("understood"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(riskTool{name: "net", risk: toolruntime.RiskNetwork, called: &called})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100, Permission: denyNetwork})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("a denied tool must not be executed")
	}
	tr := produced[1]
	if tr.Content[0].Type != provider.BlockToolResult || !tr.Content[0].IsError {
		t.Fatalf("expected an is_error tool_result, got %+v", tr.Content[0])
	}
	if !strings.Contains(tr.Content[0].ToolContent, "permission denied") {
		t.Errorf("tool_result content = %q, want it to explain the denial", tr.Content[0].ToolContent)
	}
}

// TestLoopPermissionAllowsUngatedTool verifies a tool the policy permits runs
// normally (the gate only blocks denied risks).
func TestLoopPermissionAllowsUngatedTool(t *testing.T) {
	called := false
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "reader", `{}`),
		textResponse("done"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(riskTool{name: "reader", risk: toolruntime.RiskReadOnly, called: &called})
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100, Permission: denyNetwork})

	if _, err := loop.Run(context.Background(), nil, &memEmitter{}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("a permitted tool should execute")
	}
}
