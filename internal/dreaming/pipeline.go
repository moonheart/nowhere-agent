package dreaming

import (
	"strings"

	"nowhere-agent/internal/provider"
)

// Structured-output schemas (capability L3). The dreaming stages force the
// model to return a JSON object via a forced tool call, which structurally
// excludes reasoning prose from the payload — the answer is a tool_use block's
// JSON input, not free text. This is what stops the chain-of-thought-leaks-
// into-facts bug that plain text parsing suffered.

// extractResult is the structured output of the extract stage.
type extractResult struct {
	Facts []string `json:"facts"`
}

// extractSchema is the JSON Schema for the extract stage's forced response.
var extractSchema = &provider.JSONResponseSpec{
	Name:        "record_facts",
	Description: "Record durable facts/preferences about the user extracted from the transcript. Return an empty facts array when nothing is durable.",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"facts": map[string]any{
				"type":        "array",
				"description": "Durable facts/preferences about the user, one per entry. Empty if none.",
				"items":       map[string]any{"type": "string"},
			},
		},
		"required":             []string{"facts"},
		"additionalProperties": false,
	},
}

// summaryResult is the structured output of the compress stage.
type summaryResult struct {
	Summary string `json:"summary"`
}

// summarySchema is the JSON Schema for the compress stage's forced response.
var summarySchema = &provider.JSONResponseSpec{
	Name:        "record_summary",
	Description: "Record a compact running summary of this slice of conversation.",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{
				"type":        "string",
				"description": "A few-sentence summary of outcomes, decisions, and anything worth recalling later.",
			},
		},
		"required":             []string{"summary"},
		"additionalProperties": false,
	},
}

// reflectResult is the structured output of the reflect stage.
type reflectResult struct {
	Insights   []string `json:"insights"`
	Deprecate  []string `json:"deprecate"`
}

// reflectSchema is the JSON Schema for the reflect stage's forced response.
var reflectSchema = &provider.JSONResponseSpec{
	Name:        "record_reflection",
	Description: "Record cross-memory insights and the existing memories to deprecate (duplicated/superseded).",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"insights": map[string]any{
				"type":        "array",
				"description": "Higher-level cross-memory patterns or conclusions. Empty if none.",
				"items":       map[string]any{"type": "string"},
			},
			"deprecate": map[string]any{
				"type":        "array",
				"description": "Exact existing-memory texts that are now duplicated or superseded. Empty if none.",
				"items":       map[string]any{"type": "string"},
			},
		},
		"required":             []string{"insights", "deprecate"},
		"additionalProperties": false,
	},
}

// extractPrompt builds the prompt for fact/preference extraction. The response
// is constrained by extractSchema, so the prompt only states the task.
func extractPrompt(episodeText string) string {
	return `Extract durable facts and preferences worth remembering long-term about
the USER from this conversation transcript. Facts are about the user (who they
are, what they prefer, own, or are working on), NOT about this conversation or
your task. Skip transient detail, small talk, and task narration.

TRANSCRIPT:
` + episodeText
}

// summaryPrompt builds the prompt for compressing one incremental batch of
// episodes into a compact summary (memory.KindSummary). The batch is only the
// messages beyond the session's watermark, so the summary captures what
// happened in THIS slice — not a re-summary of the whole conversation.
func summaryPrompt(episodeText string) string {
	return `Summarize this slice of a conversation. Capture outcomes, decisions,
and anything worth recalling later; drop filler and transient detail.

TRANSCRIPT:
` + episodeText
}

// reflectPrompt builds the prompt for the reflect stage: given the new batch
// summary and the user's existing long-term memories, derive higher-level
// cross-memory patterns (memory.KindInsight) and flag redundant/contradicted
// memories to deprecate. Reflection is what deduplicates the facts the
// incremental extractor re-derives across batches (reorganize only catches
// contradictions, not duplicates).
func reflectPrompt(summary string, existing []string) string {
	var b strings.Builder
	b.WriteString(`You are consolidating an agent's long-term memory about a user.

EXISTING MEMORIES (one per line):
`)
	for _, m := range existing {
		b.WriteString("- " + m + "\n")
	}
	b.WriteString(`
NEW EPISODE SUMMARY:
` + summary + `

Derive higher-level insights (patterns spanning multiple memories or following
from the new summary) and list the exact existing-memory texts that are now
duplicated or superseded. Keep insights few and genuinely non-obvious; do not
restate existing facts.`)
	return b.String()
}

// contradicts reports whether a new fact contradicts an existing memory.
// Heuristic for now: identical normalized content means a duplicate (skip),
// and an explicit "no longer"/negation marker signals contradiction. Real
// semantic contradiction detection is an LLM task — kept simple here.
func contradicts(existing, newFact string) bool {
	e := strings.ToLower(strings.TrimSpace(existing))
	n := strings.ToLower(strings.TrimSpace(newFact))
	if e == n {
		return false
	}
	// Crude negation heuristic: new fact negates a key phrase of the old.
	if strings.Contains(n, "no longer") || strings.Contains(n, "not anymore") {
		return true
	}
	return false
}
