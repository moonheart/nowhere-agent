package builtin

import (
	"context"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// ProgressUIToolName is the built-in tool that demos live progress UI: it runs
// a simulated task and pushes a progress card that updates in place while it
// works. The final spec also rides the Result, so a reload re-renders the
// settled card.
const ProgressUIToolName = "ui_progress"

// progressStages is the simulated pipeline the tool walks through.
var progressStages = []string{"waking up", "scanning files", "crunching numbers", "polishing", "done"}

// progressSpec builds the card spec for step n/total.
func progressSpec(n, total int, stage string) *provider.GenerativeUISpec {
	percent := 0
	if total > 0 {
		percent = n * 100 / total
	}
	return &provider.GenerativeUISpec{Root: []provider.GenerativeUINode{
		{
			Component: "test-ui-card",
			Props: map[string]any{
				"title":   "Progress",
				"body":    "A simulated long task, updating live from inside the tool.",
				"variant": "info",
				"percent": percent,
				"stage":   stage,
			},
		},
	}}
}

// progressUITool lets the model demo live agent-driven UI: the tool pushes a
// progress card via the loop's generative-UI pusher on every stage. RiskReadOnly:
// it changes no state, only renders UI.
type progressUITool struct{}

// NewProgressUI returns the ui_progress tool (live progress-card demo).
func NewProgressUI() toolruntime.Tool { return progressUITool{} }

func (progressUITool) Name() string { return ProgressUIToolName }
func (progressUITool) Description() string {
	return "Run a simulated long task with a progress card that updates live (agent-driven UI demo). " +
		"Call this tool when the user asks to demo or test live progress UI. It takes no arguments."
}
func (progressUITool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (progressUITool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (progressUITool) Timeout() time.Duration { return 15 * time.Second }

func (progressUITool) Call(ctx context.Context, _ map[string]any) (toolruntime.Result, error) {
	push := toolruntime.GenerativeUIFrom(ctx)
	total := len(progressStages)
	for i, stage := range progressStages {
		select {
		case <-ctx.Done():
			return toolruntime.Result{Content: "ui_progress was cancelled", IsError: true}, nil
		case <-time.After(300 * time.Millisecond):
		}
		if push != nil {
			push(progressSpec(i+1, total, stage))
		}
	}
	return toolruntime.Result{Content: "progress demo complete", GenerativeUI: progressSpec(total, total, "done")}, nil
}
