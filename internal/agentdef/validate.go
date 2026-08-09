package agentdef

import "fmt"

// Validate parses an agent definition document and enforces the authoring
// rules shared by the management API and any importer: the frontmatter must
// parse (which already requires a name and a when-to-use description) and the
// body — the child's system prompt — must be non-empty. It is the write-path
// counterpart of Parse, which stays tolerant for loaders.
func Validate(doc string) (AgentDef, error) {
	d, err := Parse(doc)
	if err != nil {
		return AgentDef{}, err
	}
	if d.System == "" {
		return AgentDef{}, fmt.Errorf("agent body (system prompt) is required")
	}
	return d, nil
}
