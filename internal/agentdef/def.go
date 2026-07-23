// Package agentdef defines subagent types (design: subagent capability). An
// AgentDef is a named agent configuration — system prompt, scoped tool
// allow/deny lists, model, and referenced skills — resolvable across
// system/team/user scopes. Definitions author the child loops that the
// spawn_agent tool launches; the loop itself never depends on where a
// definition came from.
package agentdef

import "nowhere-agent/internal/identity"

// AgentDef is a subagent type. The body of its source document is the child's
// system prompt; the frontmatter fields scope its tools and model.
type AgentDef struct {
	// Name is the unique type identifier used as subagent_type.
	Name string
	// WhenToUse is the one-line description that tells the model when to pick
	// this agent (frontmatter `description`).
	WhenToUse string
	// Tools is the allow-list of tool names. Nil or ["*"] inherits the full
	// (filtered) parent pool.
	Tools []string
	// DisallowedTools is removed from the pool after allow resolution.
	DisallowedTools []string
	// Model names the model the child loop uses; empty inherits the parent run's.
	Model string
	// MaxTurns caps the child loop's iterations; 0 uses the loop default.
	MaxTurns int
	// Skills lists skill names whose registered script tools are added to the
	// child's allow-list (only meaningful when Tools is an explicit allow-list).
	Skills []string
	// System is the child's system prompt (the document body).
	System string
	// Scope is where this definition lives (system/team/user).
	Scope identity.ScopeRef
}

// Wildcard reports whether the definition inherits the full filtered tool pool
// (no explicit allow-list).
func (d AgentDef) Wildcard() bool {
	return len(d.Tools) == 0 || (len(d.Tools) == 1 && d.Tools[0] == "*")
}

// GeneralPurpose is the always-available default agent type.
const GeneralPurpose = "general-purpose"

// Builtins returns the definitions that ship in code, available before any
// user/team/system document is authored. They live at system scope and are
// overridable by a same-named scoped definition.
func Builtins() []AgentDef {
	return []AgentDef{
		{
			Name:      GeneralPurpose,
			WhenToUse: "General-purpose agent for researching complex questions, searching code, and executing multi-step tasks. Use when a task is self-contained and its intermediate work is not worth keeping in the main context.",
			// Wildcard tools (inherit the parent pool), inherit the parent model.
			System: "You are a subagent of nowhere-agent, launched to handle one self-contained task. " +
				"You do not see the parent conversation — work only from the task prompt you were given. " +
				"Use your tools to investigate and act, then finish with a single concise message that reports " +
				"your result or findings. That final message is the only thing returned to the agent that launched you, " +
				"so make it self-contained: state the answer, the files or evidence involved, and anything the caller must know. " +
				"Do not ask questions — you cannot receive a reply.",
			Scope: identity.SystemScope(),
		},
	}
}
