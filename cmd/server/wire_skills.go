package main

import (
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/settings"
	"nowhere-agent/internal/skill"
)

// wire_skills.go — the memory port, skill engine, agent-definition store, and
// the chat context builder (base system prompt + skill L0 index + recalled
// memories). Extracted verbatim from run() (see deps.go).

func (d *serverDeps) wireSkillsAndMemory() {
	// Memory (PG+vector) and skill engine feed the loop's system prompt:
	// L0 skill index + recalled memories, scoped to the caller (task 4.5).
	d.memPort = memory.NewPGPort(d.pool)
	d.skillStore = skill.NewPGStore(d.pool)
	// Durable agent definitions (persist-agent-defs): one PG store backs both
	// the spawn resolver (inside the provider branch) and the management API
	// (outside it, so the console works with no LLM configured).
	d.agentDefPG = agentdef.NewPGStore(d.pool)
	// Skills are managed entirely through the skill console (skillapi) now —
	// the old SKILLS_DIR disk-seed path was removed, so there is no boot-time
	// import to keep in sync with the management surface.
	d.skillEngine = skill.NewEngine(d.skillStore)
	d.ctxBuilder = chatapi.NewContextBuilder(d.baseSystemFor, d.identitySvc, d.memPort, d.skillEngine)
}

// baseSystemFor resolves the base system prompt per request from the runtime
// settings (system prompt language, LLM_SYSTEM_LANG / admin settings):
// Chinese-first deployments set "zh" so the chat base prompt and the built-in
// subagent definition are phrased for Chinese users. Custom prompts (skills,
// agent definitions, system_prompt overrides) always win over these defaults.
// Resolved PER REQUEST so switching the language in the admin console applies
// to the next run — no restart.
func (d *serverDeps) baseSystemFor() string {
	if d.settings.String(settings.KeySystemLang) == "zh" {
		return "你是 nowhere-agent,一名乐于助人的 AI 助手。请用中文思考并回复,除非用户明确要求其他语言。"
	}
	return "You are nowhere-agent, a helpful AI assistant."
}
