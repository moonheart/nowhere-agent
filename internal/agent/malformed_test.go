package agent

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// TestAccumulatorFinalizeMalformedToolJSON pins the low-level signal: malformed
// tool-call JSON surfaces as an error from finalize (valid JSON does not).
func TestAccumulatorFinalizeMalformedToolJSON(t *testing.T) {
	bad := &accumulator{block: provider.Block{Type: provider.BlockToolUse, ToolUseID: "x", ToolName: "echo"}, json: "{oops"}
	if _, err := bad.finalize(); err == nil {
		t.Error("expected an error for malformed tool JSON")
	}
	good := &accumulator{block: provider.Block{Type: provider.BlockToolUse, ToolUseID: "x", ToolName: "echo"}, json: `{"x":1}`}
	blk, err := good.finalize()
	if err != nil {
		t.Fatalf("valid JSON should not error: %v", err)
	}
	if blk.ToolInput["x"] != float64(1) {
		t.Errorf("tool input = %+v", blk.ToolInput)
	}
}

// TestLoopMalformedToolArgsBecomesErrorResult verifies a tool call with
// unparseable arguments is not dispatched but fed back as an is_error
// tool_result, and the loop then continues to a final answer.
func TestLoopMalformedToolArgsBecomesErrorResult(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{
		toolUseResponse("tu1", "echo", `{bad json`),
		textResponse("recovered"),
	}}
	reg := toolruntime.NewRegistry()
	reg.Register(echoTool{}) // echo would return a non-error result if (wrongly) dispatched
	loop := New(p, reg, Config{Model: "m", MaxTokens: 100})

	produced, err := loop.Run(context.Background(), nil, &memEmitter{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// assistant(tool_use) + user(tool_result, is_error) + assistant(final)
	if len(produced) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(produced), produced)
	}
	tr := produced[1]
	if tr.Role != provider.RoleUser || len(tr.Content) == 0 || tr.Content[0].Type != provider.BlockToolResult {
		t.Fatalf("msg1 should be a tool_result, got %+v", tr)
	}
	if !tr.Content[0].IsError {
		t.Error("malformed tool args should yield an is_error tool_result, not a dispatched call")
	}
	if !strings.Contains(tr.Content[0].ToolContent, "invalid tool arguments") {
		t.Errorf("tool_result content = %q, want it to name the invalid arguments", tr.Content[0].ToolContent)
	}
	if produced[2].Content[0].Text != "recovered" {
		t.Errorf("final = %q, want the loop to continue after self-correcting", produced[2].Content[0].Text)
	}
}
