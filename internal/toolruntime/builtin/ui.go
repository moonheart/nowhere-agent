package builtin

import (
	"context"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// TestUIToolName is the built-in tool that pushes a fixed generative-UI card to
// the client (agent-driven UI smoke test). The card is a placeholder for
// whatever UI a future tool wants to declare; the frontend renders the spec
// through its allowlist.
const TestUIToolName = "test_ui"

// testUISpec is the fixed card the tool renders: a title, a body, a variant,
// and a bullet list. Component name "test-ui-card" is resolved by the client's
// allowlist.
func testUISpec() *provider.GenerativeUISpec {
	return &provider.GenerativeUISpec{Root: []provider.GenerativeUINode{
		{
			Component: "test-ui-card",
			Props: map[string]any{
				"title":   "Generative UI works",
				"body":    "This card was pushed by the test_ui tool through the agent event stream.",
				"variant": "success",
			},
			Children: []provider.GenerativeUINode{
				{Component: "test-ui-bullet", Props: map[string]any{"text": "The spec travels as a durable message block."}},
				{Component: "test-ui-bullet", Props: map[string]any{"text": "History reloads re-render it from the message store."}},
				{Component: "test-ui-bullet", Props: map[string]any{"text": "Unknown component names render nothing (allowlist)."}},
			},
		},
	}}
}

// testUITool lets the model push a fixed test UI card. RiskReadOnly: it changes
// no state, only declares UI the client renders.
type testUITool struct{}

// NewTestUI returns the test_ui tool (generative-UI smoke test).
func NewTestUI() toolruntime.Tool { return testUITool{} }

func (testUITool) Name() string { return TestUIToolName }
func (testUITool) Description() string {
	return "Push a fixed test UI card into the conversation (agent-driven UI smoke test). " +
		"Call this tool when the user asks to test or demo UI rendering. It takes no arguments."
}
func (testUITool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (testUITool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (testUITool) Timeout() time.Duration { return 15 * time.Second }

func (testUITool) Call(_ context.Context, _ map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{
		Content:      "test UI card pushed",
		GenerativeUI: testUISpec(),
	}, nil
}
