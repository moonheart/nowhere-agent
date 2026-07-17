package skill

import (
	"bufio"
	"fmt"
	"strings"
)

// ParseManifest parses a SKILL.md document: a YAML-lite frontmatter block
// delimited by "---" lines (supporting name and description keys) followed by
// the markdown body.
func ParseManifest(doc string) (Manifest, error) {
	var m Manifest
	sc := bufio.NewScanner(strings.NewReader(doc))

	// Expect opening frontmatter delimiter.
	if !sc.Scan() {
		return m, fmt.Errorf("empty skill document")
	}
	if strings.TrimSpace(sc.Text()) != "---" {
		// No frontmatter; treat whole doc as body, name unknown.
		m.Body = strings.TrimSpace(doc)
		return m, nil
	}

	// Read frontmatter until closing delimiter.
	closed := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			closed = true
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			m.Name = strings.TrimSpace(v)
		case "description":
			m.Description = strings.TrimSpace(v)
		}
	}
	if !closed {
		return m, fmt.Errorf("unterminated frontmatter")
	}

	// Rest is the body.
	var body strings.Builder
	for sc.Scan() {
		body.WriteString(sc.Text())
		body.WriteString("\n")
	}
	m.Body = strings.TrimSpace(body.String())
	if err := sc.Err(); err != nil {
		return m, err
	}
	if m.Name == "" {
		return m, fmt.Errorf("skill name is required")
	}
	return m, nil
}
