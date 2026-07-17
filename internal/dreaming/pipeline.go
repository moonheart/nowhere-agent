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
