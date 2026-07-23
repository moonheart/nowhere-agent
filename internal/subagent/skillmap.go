package subagent

import (
	"strings"

	"nowhere-agent/internal/toolruntime"
)

// skillToolNames resolves skill names to the registered script-tool names that
// carry them. A skill's L2 scripts are registered as tools named
// "skill_<skill>_<script>" (see skill.ScriptTool). An agent definition that
// lists a skill therefore gains that skill's script tools in its scoped pool —
// there is no prompt preloading; a subagent "uses a skill" by calling its tool.
func skillToolNames(reg *toolruntime.Registry, skills []string) []string {
	if len(skills) == 0 {
		return nil
	}
	names := reg.Names()
	var out []string
	for _, sk := range skills {
		prefix := "skill_" + sanitize(sk) + "_"
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				out = append(out, n)
			}
		}
	}
	return out
}

// sanitize mirrors skill.ScriptTool's name sanitization: non-alphanumeric,
// non-underscore runes become underscores.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
