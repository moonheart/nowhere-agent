package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/toolruntime"
)

// PlanStateKey is the session-state dictionary key the plan_write tool writes
// (capability-gap O1). The frontend's plan panel reads this key; other features
// use their own keys, so the single state column stays extensible.
const PlanStateKey = "plan"

// PlanItem is one task in the agent's plan. Status is one of pending,
// in_progress, completed; ActiveForm is the present-tense label shown while the
// task is in_progress (e.g. "Reading config files").
type PlanItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm,omitempty"`
}

// plan is the value stored under PlanStateKey.
type plan struct {
	Items []PlanItem `json:"items"`
}

// PlanWriteToolName is the built-in tool the model calls to record/update its
// working plan (capability-gap O1). The full plan is rewritten each call, so the
// stored value is always the authoritative current plan.
const PlanWriteToolName = "plan_write"

// planWriteTool lets the model maintain a visible task list. Each call replaces
// the whole plan (the model re-submits every item with its current status), and
// the writer persists it under PlanStateKey in the session state store and fans
// it out live so the client's plan panel updates in real time. RiskReadOnly: it
// touches no workspace files and no external system, only the session's own
// state dictionary, so the permission gate leaves it be.
type planWriteTool struct {
	write agent.SessionStateWriter
}

// NewPlanWrite returns the plan_write tool bound to a session-state writer. The
// writer is the low-coupling seam: the tool knows nothing about the session
// store, SQL, or the broker — it just calls write(ctx, PlanStateKey, plan).
func NewPlanWrite(write agent.SessionStateWriter) toolruntime.Tool {
	return &planWriteTool{write: write}
}

func (t *planWriteTool) Name() string { return PlanWriteToolName }
func (t *planWriteTool) Description() string {
	return "Record and update your working plan as a task list the user can watch. " +
		"Use it for any multi-step task: break the work into steps, then update statuses as you progress. " +
		"Each call REPLACES the whole plan — always submit every task with its current status. " +
		"Mark exactly one task in_progress (the one you are doing now); keep the rest pending or completed."
}
func (t *planWriteTool) Schema() map[string]any {
	item := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content":    map[string]any{"type": "string", "description": "The task, as a short imperative (e.g. \"Read the config loader\")."},
			"status":     map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}, "description": "Current state of the task."},
			"activeForm": map[string]any{"type": "string", "description": "Present-tense label shown while in_progress (e.g. \"Reading the config loader\")."},
		},
		"required": []string{"content", "status"},
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":        "array",
				"description": "The full plan, rewritten each call. Every task with its current status.",
				"items":       item,
			},
		},
		"required": []string{"items"},
	}
}
func (t *planWriteTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (t *planWriteTool) Timeout() time.Duration { return 15 * time.Second }

func (t *planWriteTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	p, res, err := parsePlan(args)
	if err != nil {
		return res, nil
	}
	if t.write == nil {
		return toolruntime.Result{Content: "plan_write is not wired to a session-state store", IsError: true}, nil
	}
	if err := t.write(ctx, PlanStateKey, p); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("plan_write failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: renderPlan(p)}, nil
}

// parsePlan validates and decodes the plan argument. It returns an error Result
// (not a Go error) so the model can correct malformed input.
func parsePlan(args map[string]any) (plan, toolruntime.Result, error) {
	raw, ok := args["items"]
	if !ok {
		return plan{}, toolruntime.Result{Content: "missing required argument \"items\"", IsError: true}, fmt.Errorf("no items")
	}
	list, ok := raw.([]any)
	if !ok {
		return plan{}, toolruntime.Result{Content: "argument \"items\" must be an array", IsError: true}, fmt.Errorf("bad items")
	}
	p := plan{Items: make([]PlanItem, 0, len(list))}
	inProgress := 0
	for i, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			return plan{}, toolruntime.Result{Content: fmt.Sprintf("items[%d] must be an object", i), IsError: true}, fmt.Errorf("bad item")
		}
		content, _ := m["content"].(string)
		if strings.TrimSpace(content) == "" {
			return plan{}, toolruntime.Result{Content: fmt.Sprintf("items[%d].content must be a non-empty string", i), IsError: true}, fmt.Errorf("empty content")
		}
		status, _ := m["status"].(string)
		switch status {
		case "pending", "in_progress", "completed":
		default:
			return plan{}, toolruntime.Result{Content: fmt.Sprintf("items[%d].status must be pending|in_progress|completed (got %q)", i, status), IsError: true}, fmt.Errorf("bad status")
		}
		if status == "in_progress" {
			inProgress++
		}
		activeForm, _ := m["activeForm"].(string)
		p.Items = append(p.Items, PlanItem{Content: content, Status: status, ActiveForm: activeForm})
	}
	if inProgress > 1 {
		return plan{}, toolruntime.Result{Content: fmt.Sprintf("%d tasks are in_progress; mark at most one in_progress", inProgress), IsError: true}, fmt.Errorf("too many in_progress")
	}
	return p, toolruntime.Result{}, nil
}

// renderPlan renders the plan back to the model as a compact checklist, so the
// tool result confirms what was recorded.
func renderPlan(p plan) string {
	var b strings.Builder
	done := 0
	for _, it := range p.Items {
		mark := "○"
		switch it.Status {
		case "in_progress":
			mark = "◐"
		case "completed":
			mark = "●"
			done++
		}
		label := it.Content
		if it.Status == "in_progress" && it.ActiveForm != "" {
			label = it.ActiveForm
		}
		fmt.Fprintf(&b, "%s %s\n", mark, label)
	}
	fmt.Fprintf(&b, "(%d/%d completed)", done, len(p.Items))
	return b.String()
}
