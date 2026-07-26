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
// is constrained by extractSchema; the prompt states the task and instructs the
// model to answer ONLY via the record_facts function (the gateway rejects a
// forced tool_choice, so we soft-force by instruction). `today` anchors time:
// transcript lines carry [date time] prefixes, and facts must absolute relative
// time ("next Saturday", "recently") against it so they stay interpretable later.
func extractPrompt(episodeText, today string) string {
	return `Extract durable facts and preferences worth remembering long-term about
the USER from this conversation transcript. Facts are about the user (who they
are, what they prefer, own, or are working on), NOT about this conversation or
your task. Skip transient detail, small talk, and task narration.

Today's date is ` + today + `. Each transcript line is prefixed with the date/time
it was said. When a fact involves time (a plan, a deadline, a current location or
state), ABSOLUTE it: convert "next Saturday" / "recently" / "I'm in Singapore"
into explicit dates or an as-of clause (e.g. "as of ` + today + `, ..."), so the
fact stays correct and interpretable after time passes.

You MUST respond ONLY by calling the record_facts function with the facts array
(empty if nothing is durable). Do not write any normal text answer.

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

You MUST respond ONLY by calling the record_summary function with the summary.
Do not write any normal text answer.

TRANSCRIPT:
` + episodeText
}

// reflectPrompt builds the prompt for the reflect stage: given the new batch
// summary and the user's existing long-term memories, derive higher-level
// cross-memory patterns (memory.KindInsight) and flag redundant/contradicted
// memories to deprecate. `today` lets reflection judge time-staleness (a plan
// whose date has passed is superseded even without an explicit contradiction).
// Reflection is what deduplicates the facts the incremental extractor
// re-derives across batches (reorganize only catches contradictions).
func reflectPrompt(summary string, existing []string, today string) string {
	var b strings.Builder
	b.WriteString(`You are consolidating an agent's long-term memory about a user.
Today's date is ` + today + ` — take the passage of time into account: a memory
about a transient state (an upcoming plan, a current location, an in-progress
task) may be stale even if nothing explicitly contradicts it.

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
duplicated, superseded, or time-stale. Keep insights few (at most 3), SHORT (one
sentence each), and genuinely non-obvious; do not restate existing facts.

You MUST respond ONLY by calling the record_reflection function with the
insights and deprecate arrays (empty if none). Do not write any normal text
answer.`)
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

// reviseResult is the structured output of the reorganize (revise) stage.
type reviseResult struct {
	Deprecate []string `json:"deprecate"`
	Rewrite   string   `json:"rewrite"`
}

// reviseSchema is the JSON Schema for the reorganize stage's forced response.
var reviseSchema = &provider.JSONResponseSpec{
	Name:        "revise_memory",
	Description: "Decide how a new fact revises existing long-term memories: which to deprecate (contradicted or time-stale), and the fact's final (time-corrected) wording.",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deprecate": map[string]any{
				"type":        "array",
				"description": "Exact existing-memory texts this fact contradicts or makes time-stale. Empty if none.",
				"items":       map[string]any{"type": "string"},
			},
			"rewrite": map[string]any{
				"type":        "string",
				"description": "The new fact reworded with time made absolute/correct (e.g. 'going to Singapore' → 'went to Singapore in July 2026'). Empty to store the fact as-is.",
			},
		},
		"required":             []string{"deprecate", "rewrite"},
		"additionalProperties": false,
	},
}

// revisePrompt builds the prompt for the reorganize (revise) stage: given one
// new fact, the scope's live memories, and today's date, decide which memories
// the fact contradicts or makes time-stale, and whether the fact itself needs
// time-correcting before it is stored. This is the 随时间保鲜 capability: a
// memory can go stale from the passage of time alone (a plan whose date passed,
// a place the user has left), not only from an explicit contradiction.
func revisePrompt(fact string, existing []string, today string) string {
	var b strings.Builder
	b.WriteString(`You are maintaining an agent's long-term memory about a user.
Today's date is ` + today + `.

EXISTING MEMORIES (one per line):
`)
	if len(existing) == 0 {
		b.WriteString("(none)\n")
	}
	for _, m := range existing {
		b.WriteString("- " + m + "\n")
	}
	b.WriteString(`
NEW FACT:
` + fact + `

Decide:
1. deprecate — the exact existing-memory texts that this new fact contradicts
   OR makes time-stale. A memory is time-stale when, given today's date, the
   state it describes has passed: an upcoming plan whose date is now past
   ("planning a party for next Saturday" when that Saturday has come), a current
   location the user has likely left, an in-progress task now finished. Do NOT
   deprecate stable preferences/facts that time does not invalidate.
2. rewrite — if the new fact itself describes something time-relative, reword it
   with time made absolute/correct (e.g. "is going to Singapore in July" → "went
   to Singapore in July 2026"). Leave empty to store the fact unchanged.

You MUST respond ONLY by calling the revise_memory function with the deprecate
and rewrite fields (deprecate empty, rewrite empty when nothing changes). Do not
write any normal text answer.`)
	return b.String()
}
