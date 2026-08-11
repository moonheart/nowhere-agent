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
// overridable by a same-named scoped definition. The prompts are English;
// BuiltinsForLang("zh") returns the Chinese-phrased defaults instead.
func Builtins() []AgentDef {
	return BuiltinsForLang("en")
}

// BuiltinsForLang returns the shipped default definitions with prompts in the
// requested language ("en" | "zh"; any other value falls back to English).
// The server picks the language from config so Chinese-first deployments get
// Chinese-phrased subagent instructions out of the box.
func BuiltinsForLang(lang string) []AgentDef {
	general := subagentPromptEn
	whenToUse := "General-purpose agent for researching complex questions, searching code, and executing multi-step tasks. Use when a task is self-contained and its intermediate work is not worth keeping in the main context."
	if lang == "zh" {
		general = subagentPromptZh
		whenToUse = "通用子代理:用于研究复杂问题、检索代码、执行多步任务。当任务自包含、其中间过程不值得保留在主上下文时使用。"
	}
	return []AgentDef{
		{
			Name:      GeneralPurpose,
			WhenToUse: whenToUse,
			// Wildcard tools (inherit the parent pool), inherit the parent model.
			System: general,
			Scope:  identity.SystemScope(),
		},
	}
}

const (
	subagentPromptEn = "You are a subagent of nowhere-agent, launched to handle one self-contained task. " +
		"You do not see the parent conversation — work only from the task prompt you were given. " +
		"Use your tools to investigate and act, then finish with a single concise message that reports " +
		"your result or findings. That final message is the only thing returned to the agent that launched you, " +
		"so make it self-contained: state the answer, the files or evidence involved, and anything the caller must know. " +
		"Do not ask questions — you cannot receive a reply."
	subagentPromptZh = "你是 nowhere-agent 的子代理,负责完成一件自包含的任务。" +
		"你看不到父对话——只能依据交给你的任务提示工作。" +
		"使用你的工具进行调查与行动,最后用一条简洁的消息汇报结果或发现。" +
		"这条最终消息是返回给启动你的父代理的唯一内容,所以必须自包含:写明答案、涉及的文件或证据、以及调用方需要知道的一切。" +
		"不要提问——你无法收到回复。"
)
