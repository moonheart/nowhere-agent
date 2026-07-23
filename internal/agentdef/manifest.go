package agentdef

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Parse reads an agent definition document: a YAML-lite frontmatter block
// delimited by "---" lines, followed by a markdown body used as the child's
// system prompt. Recognised frontmatter keys: name, description, model,
// maxTurns, and the comma-separated lists tools, disallowedTools, skills.
//
// List values are inline comma-separated (e.g. `tools: read_file, write_file`);
// multi-line YAML sequences are not supported (mirrors the skill manifest
// parser's YAML-lite scope).
func Parse(doc string) (AgentDef, error) {
	var d AgentDef
	sc := bufio.NewScanner(strings.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if !sc.Scan() {
		return d, fmt.Errorf("empty agent document")
	}
	if strings.TrimSpace(sc.Text()) != "---" {
		return d, fmt.Errorf("agent document requires frontmatter delimited by ---")
	}

	closed := false
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		switch key {
		case "name":
			d.Name = val
		case "description":
			d.WhenToUse = val
		case "model":
			d.Model = val
		case "tools":
			d.Tools = splitList(val)
		case "disallowedTools":
			d.DisallowedTools = splitList(val)
		case "skills":
			d.Skills = splitList(val)
		case "maxTurns":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				d.MaxTurns = n
			}
		}
	}
	if !closed {
		return d, fmt.Errorf("unterminated frontmatter")
	}
	if err := sc.Err(); err != nil {
		return d, err
	}

	var body strings.Builder
	for sc.Scan() {
		body.WriteString(sc.Text())
		body.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		return d, err
	}
	d.System = strings.TrimSpace(body.String())

	if d.Name == "" {
		return d, fmt.Errorf("agent name is required")
	}
	if d.WhenToUse == "" {
		return d, fmt.Errorf("agent description is required")
	}
	return d, nil
}

// splitList parses an inline comma-separated list, trimming empties.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
