package dreaming

import "strings"

// extractPrompt builds the LLM prompt for fact/preference extraction.
func extractPrompt(episodeText string) string {
	return `From the following conversation transcript, extract durable facts and
preferences worth remembering long-term. One per line. Skip transient detail.

TRANSCRIPT:
` + episodeText + `

FACTS (one per line):`
}

// summaryPrompt builds the LLM prompt for compressing one incremental batch of
// episodes into a compact summary (memory.KindSummary). The batch is only the
// messages beyond the session's watermark, so the summary captures what
// happened in THIS slice — not a re-summary of the whole conversation.
func summaryPrompt(episodeText string) string {
	return `Summarize the following slice of a conversation into a compact running
summary. Capture outcomes, decisions, and anything worth recalling later;
drop filler and transient detail. A few sentences.

TRANSCRIPT:
` + episodeText + `

SUMMARY:`
}

// reflectPrompt builds the LLM prompt for the reflect stage: given the new
// batch summary and the user's existing long-term memories, derive higher-level
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

From these, output:
- Higher-level INSIGHTS: patterns or conclusions that span multiple memories
  or follow from the new summary. One per line, prefixed "insight: ".
- Deprecations: EXACT existing-memory lines that are now duplicated or
  superseded, one per line, prefixed "deprecate: ". Omit this section if none.
Keep insights few and genuinely non-obvious; do not restate existing facts.

OUTPUT:`)
	return b.String()
}

// splitLines splits LLM output into trimmed non-empty lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if line != "" {
			out = append(out, line)
		}
	}
	return out
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
