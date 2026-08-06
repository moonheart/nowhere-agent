package subagent

import (
	"nowhere-agent/internal/skill"
	"nowhere-agent/internal/toolruntime"
)

// skillToolNames maps an agent definition's declared skills to the tools that
// run them. Skills are executed by the single fixed run_skill_script tool (see
// skill.RunSkillScriptTool): the tool resolves a script by name against the
// caller's scopes at call time, so there are no per-script `skill_*` tools to
// enumerate. A definition that lists any skill therefore just needs that one
// tool in its scoped pool — the tool itself enforces scope when the child calls
// it. Returns nil when the definition declares no skills or the parent run has
// no script runner (e.g. exec disabled, or no visible skill has scripts).
func skillToolNames(reg *toolruntime.Registry, skills []string) []string {
	if len(skills) == 0 {
		return nil
	}
	if _, ok := reg.Get(skill.RunSkillScriptName); !ok {
		return nil
	}
	return []string{skill.RunSkillScriptName}
}
