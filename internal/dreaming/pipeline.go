package dreaming

import (
	"fmt"
	"sort"
	"strings"

	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
)

// Structured-output schemas (capability L3). Every stage forces the model to
// return a JSON object via a forced tool call, which structurally excludes
// reasoning prose from the payload — the answer is a tool_use block's JSON
// input, not free text. This is what stops the chain-of-thought-leaks-into-
// facts bug that plain text parsing suffered.

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

// ---- consolidation (memory-consolidation) ----

// updateOp revises an existing memory in place, addressed by its handle.
type updateOp struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// addOp introduces a new memory.
type addOp struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// removeOp retires an existing memory (deprecate, not erase).
type removeOp struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// consolidateResult is the structured output of the consolidate stage: the
// edits to apply to the scope's store.
type consolidateResult struct {
	Update []updateOp `json:"update"`
	Add    []addOp    `json:"add"`
	Remove []removeOp `json:"remove"`
}

// consolidateSchema is the JSON Schema for the consolidate stage's forced
// response. `reason` on a removal is required: it costs one short string and
// makes the pass auditable in the log, where a bare list of retired handles
// would say nothing about why the store shrank.
var consolidateSchema = &provider.JSONResponseSpec{
	Name:        "record_consolidation",
	Description: "Record how the new material folds into the existing long-term memory: which memories to revise, which to add, which to retire.",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"update": map[string]any{
				"type":        "array",
				"description": "Existing memories to rewrite in place. Empty if none.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":      map[string]any{"type": "string", "description": "The memory's handle, e.g. M7."},
						"content": map[string]any{"type": "string", "description": "The memory's new full text."},
					},
					"required":             []string{"id", "content"},
					"additionalProperties": false,
				},
			},
			"add": map[string]any{
				"type":        "array",
				"description": "New memories to store. Empty if the new material is already covered by updates.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind": map[string]any{
							"type": "string",
							"enum": []string{
								string(memory.KindFact), string(memory.KindPreference),
								string(memory.KindInsight), string(memory.KindSummary),
							},
						},
						"content": map[string]any{"type": "string"},
					},
					"required":             []string{"kind", "content"},
					"additionalProperties": false,
				},
			},
			"remove": map[string]any{
				"type":        "array",
				"description": "Existing memories to retire, because they were merged into another, superseded, or have gone stale. Empty if none.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":     map[string]any{"type": "string", "description": "The memory's handle, e.g. M7."},
						"reason": map[string]any{"type": "string", "description": "Short reason, e.g. 'merged into M3'."},
					},
					"required":             []string{"id", "reason"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"update", "add", "remove"},
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
// episodes into a compact summary. The batch is only the messages beyond the
// session's watermark, so the summary captures what happened in THIS slice —
// not a re-summary of the whole conversation. The summary is not stored
// directly: it is new material handed to consolidation, which decides whether
// it becomes a memory of its own or merges into an existing one.
func summaryPrompt(episodeText string) string {
	return `Summarize this slice of a conversation. Capture outcomes, decisions,
and anything worth recalling later; drop filler and transient detail.

You MUST respond ONLY by calling the record_summary function with the summary.
Do not write any normal text answer.

TRANSCRIPT:
` + episodeText
}

// handled pairs a memory with the short handle the model addresses it by.
type handled struct {
	handle string
	mem    memory.Memory
}

// consolidatePrompt builds the single consolidation prompt: the scope's entire
// live memory set (labelled with handles, grouped by kind, each group showing
// its cap and current count) plus the new material from this batch.
//
// Handles rather than uuids: a uuid is 36 characters, and 150 of them is 5.4KB
// of prompt spent on random strings the model must copy exactly — where a
// transposition yields a VALID OTHER id. "M7" is short, and a mistake yields an
// unknown handle the worker can ignore instead of silently editing the wrong
// memory.
func consolidatePrompt(facts []string, summary string, existing []handled, caps Caps, today string) string {
	var b strings.Builder
	b.WriteString(`You are maintaining an agent's long-term memory about ONE user.
Today's date is ` + today + `.

EXISTING MEMORIES — the complete store for this user.
`)
	for _, k := range []memory.Kind{memory.KindFact, memory.KindPreference, memory.KindInsight, memory.KindSummary} {
		writeGroup(&b, existing, caps, k)
	}

	b.WriteString("\nNEW MATERIAL FROM THE LATEST CONVERSATION\n")
	if len(facts) == 0 {
		b.WriteString("facts: (none)\n")
	} else {
		b.WriteString("facts:\n")
		for _, f := range facts {
			b.WriteString("- " + f + "\n")
		}
	}
	if strings.TrimSpace(summary) == "" {
		b.WriteString("summary: (none)\n")
	} else {
		b.WriteString("summary: " + summary + "\n")
	}

	// With nothing new, "fold the new material in" is an instruction with no
	// object, and a model handed it tends to answer with empty arrays. This is
	// the compaction pass — the store itself is the work.
	if len(facts) == 0 && strings.TrimSpace(summary) == "" {
		b.WriteString(`
There is NO new material this time. Your task is to review the store above on
its own terms and clean it up. Look for:
- duplicates and near-duplicates. The same fact written in DIFFERENT LANGUAGES
  is still one fact: "The user has a cat named Doudou (豆豆)" and "用户养了一只
  叫豆豆的猫" must become a single memory, not two.
- memories time has made stale, judged against today's date
- memories that are not about the user at all (task narration, notes about the
  assistant's own behaviour, commentary about this memory system) — retire them
- any group holding more than its cap

Merge by UPDATING the memory that should survive and REMOVING the others. If the
store is genuinely clean, return empty arrays.
`)
	} else {
		b.WriteString(`
Fold the new material into the store and return the edits.
`)
	}

	b.WriteString(`
FIDELITY — this is the hard rule. Every word you write must be supported by the
memories listed above or by the new material. You are reorganizing what is
already known; you are NOT recalling, inferring, or improving it.
- NEVER introduce a name, number, date, place, or event that does not appear in
  the source you are rewriting. If three memories say the cat is named 豆豆,
  the merged memory says 豆豆 — you have no other source for that name.
- NEVER invent a change of state. Do not write that the user "corrected",
  "updated", "clarified", or "changed" anything unless the new material actually
  shows them doing it. Two memories disagreeing is not the user correcting
  themselves; it is two memories disagreeing.
- When sources genuinely conflict and nothing resolves it, keep the more recent
  wording and retire the other. Do not synthesize a third version.
- Rewriting for brevity or to merge duplicates is expected. Adding information
  is not.

SUBJECT — every memory describes the USER: who they are, what they prefer, own,
believe, or are working on. Never write a memory about the assistant's
behaviour, about the conversation as an artifact ("the user repeated this
prompt", "this episode was recorded twice"), or about this memory system and its
consolidation. Those are observations about machinery, not knowledge about a
person, and they are worthless to recall later. If the new material yields
nothing about the user, return empty arrays — that is a correct answer.

PREFER REVISION OVER ACCUMULATION:
- when the new material refines, extends or corrects an existing memory, UPDATE
  that memory rather than adding a near-duplicate beside it
- when two existing memories say the same thing, UPDATE the better one to carry
  the merged content and REMOVE the other
- REMOVE a memory that is superseded, or that time has made stale: a plan whose
  date has passed, a location the user has left, a task now finished. Do NOT
  remove stable facts and preferences that time does not invalidate.
- keep insights few and genuinely non-obvious; an insight that merely restates
  facts already in the store is not worth its slot

CAPS — each group above shows its live count and cap. A group at its cap has no
room: to add there you must remove or merge something first. Exceeding a cap
does not work — the oldest memories in that group are evicted automatically, so
choosing what to merge yourself gives a better result than letting age decide.

TIME — write memories with absolute time ("went to Singapore in July 2026"), not
relative time ("is going next month"), so they stay interpretable later.

You MUST respond ONLY by calling the record_consolidation function with the
update, add and remove arrays (any of them empty when there is nothing to do).
Do not write any normal text answer.`)
	return b.String()
}

// writeGroup renders one kind's memories with its cap and current count. An
// empty group is still listed, so the model can see the kind exists and has
// room rather than having to infer it from silence.
func writeGroup(b *strings.Builder, existing []handled, caps Caps, kind memory.Kind) {
	var group []handled
	for _, h := range existing {
		if h.mem.Kind == kind {
			group = append(group, h)
		}
	}
	// Oldest first, so the ones nearest eviction read top-down.
	sort.SliceStable(group, func(i, j int) bool {
		return group[i].mem.CreatedAt.Before(group[j].mem.CreatedAt)
	})

	limit, shared := caps.forKind(kind)
	header := string(kind)
	if shared {
		// fact and preference draw on one pool; showing each its own count
		// against the shared cap would read as two independent budgets.
		header += " (shares one pool with " + string(memory.KindFact) + "/" + string(memory.KindPreference) + ")"
	}
	fmt.Fprintf(b, "\n%s — %d live of %d allowed:\n", header, countKind(existing, capGroupOf(kind)), limit)
	if len(group) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	for _, h := range group {
		fmt.Fprintf(b, "  %s: %s\n", h.handle, h.mem.Content)
	}
}
