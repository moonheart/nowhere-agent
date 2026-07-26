package builtin

import (
	"context"
	"fmt"
	"time"

	"nowhere-agent/internal/toolruntime"
)

// AskUserToolName is the tool the model calls to ask the user structured
// questions. It mirrors agent.AskUserToolName (the loop suspends on it); the
// constant is re-declared here so the builtin package does not import agent.
const AskUserToolName = "ask_user"

// askUser is the built-in structured-question tool (capability O-ask). The
// model calls it with 1–4 questions; the loop SUSPENDS the run on it (like a
// permission approval) and the user's answer arrives as the tool result on
// resume. Call itself is never reached in the gated path — the loop intercepts
// the call before dispatch — so it only runs if a server wires it without the
// interaction gate, where it explains the misconfiguration.
type askUser struct{}

// NewAskUser returns the structured-question tool. RiskReadOnly so the
// permission gate does not ALSO try to suspend it (the interaction gate owns it).
func NewAskUser() toolruntime.Tool { return askUser{} }

func (askUser) Name() string { return AskUserToolName }
func (askUser) Description() string {
	return "Ask the user 1–4 structured questions and wait for their answers. Use this when you " +
		"need a decision, clarification, or input only the user can give (a choice between approaches, " +
		"a missing value, a yes/no confirmation). The run PAUSES until the user answers; their response " +
		"is returned as this tool's result. Prefer offering clear options over open-ended questions."
}
func (askUser) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (askUser) Timeout() time.Duration { return 0 } // no execution timeout: it waits on a human

// Schema: 1–4 questions, each with single/multi-select options (one optionally
// the recommended default), and the user may always give a custom answer or skip.
func (askUser) Schema() map[string]any {
	option := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label":       map[string]any{"type": "string", "description": "Short option text (1–5 words)."},
			"description": map[string]any{"type": "string", "description": "What choosing this means / implies."},
			"recommended": map[string]any{"type": "boolean", "description": "Mark the default/recommended option."},
		},
		"required": []string{"label"},
	}
	question := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question":   map[string]any{"type": "string", "description": "The full question to ask."},
			"header":     map[string]any{"type": "string", "description": "Very short chip label (≤12 chars)."},
			"multiselect": map[string]any{"type": "boolean", "description": "Allow selecting multiple options."},
			"options":    map[string]any{"type": "array", "items": option, "minItems": 2, "maxItems": 4},
		},
		"required": []string{"question", "options"},
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"questions": map[string]any{
				"type":        "array",
				"description": "1–4 questions to ask, presented as one card.",
				"items":       question,
				"minItems":    1,
				"maxItems":    4,
			},
		},
		"required": []string{"questions"},
	}
}

// Call should never run in the gated path (the loop suspends first). If reached,
// the server wired the tool without the interaction gate.
func (askUser) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{
		Content: fmt.Sprintf("%s must be driven by the agent loop's interaction gate (the run suspends for the user's answer); it cannot execute directly", AskUserToolName),
		IsError: true,
	}, nil
}
