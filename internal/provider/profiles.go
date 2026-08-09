package provider

import "strings"

// ModelProfile is the static capability portrait of one model family — the
// data langchain ships from models.dev per provider. It lets the loop and the
// adapters derive behaviour that otherwise needs manual configuration: the
// context window (in-loop compression), whether sampling parameters are
// accepted (reasoning models reject temperature), and whether tool calling /
// image input are supported.
//
// The table is deliberately conservative: it covers the mainstream
// Anthropic / OpenAI(-compatible) / DeepSeek families. An unknown model is
// not an error — LookupProfile returns ok=false and every consumer keeps its
// pre-profile behaviour (compression off unless configured, all parameters
// passed through).
type ModelProfile struct {
	// ContextWindow is the model's total context window in tokens.
	ContextWindow int
	// MaxOutputTokens is the model's per-response output cap (informational;
	// adapters do not clamp to it).
	MaxOutputTokens int
	// Reasoning marks a reasoning-first model (extended thinking / o-series /
	// deepseek-reasoner).
	Reasoning bool
	// ToolCalling reports native function-calling support.
	ToolCalling bool
	// ImageInput reports native image-block support.
	ImageInput bool
	// Sampling reports that temperature/top_p are accepted. Reasoning models
	// (o-series, gpt-5, deepseek-reasoner) reject or ignore them.
	Sampling bool
}

// profileEntry keys a profile by a model-name prefix: deployments frequently
// append date or region suffixes (claude-sonnet-4-20250514, gpt-4o-2024-08-06),
// so matching is longest-prefix-first rather than exact.
type profileEntry struct {
	provider string
	prefix   string
	profile  ModelProfile
}

// modelProfiles is matched longest-prefix-first (see LookupProfile), so a
// specific entry ("gpt-4o-mini") always beats its generalisation ("gpt-4o")
// regardless of table order.
var modelProfiles = []profileEntry{
	// Anthropic — 200k window family; 4.x and 3.7 are extended-thinking
	// capable, all support tools + images + sampling.
	{"anthropic", "claude-opus-4", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 32000, Reasoning: true, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"anthropic", "claude-sonnet-4", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 64000, Reasoning: true, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"anthropic", "claude-3-7-sonnet", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 64000, Reasoning: true, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"anthropic", "claude-3-5-sonnet", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 8192, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"anthropic", "claude-3-5-haiku", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 8192, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"anthropic", "claude-3-opus", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 4096, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"anthropic", "claude-3-haiku", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 4096, ToolCalling: true, ImageInput: true, Sampling: true}},

	// OpenAI reasoning families — no sampling controls (the API rejects or
	// ignores temperature/top_p).
	{"openai", "o1-mini", ModelProfile{ContextWindow: 128000, MaxOutputTokens: 65536, Reasoning: true, ToolCalling: false, Sampling: false}},
	{"openai", "o1", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 100000, Reasoning: true, ToolCalling: true, ImageInput: true, Sampling: false}},
	{"openai", "o3-mini", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 100000, Reasoning: true, ToolCalling: true, Sampling: false}},
	{"openai", "o3", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 100000, Reasoning: true, ToolCalling: true, ImageInput: true, Sampling: false}},
	{"openai", "o4-mini", ModelProfile{ContextWindow: 200000, MaxOutputTokens: 100000, Reasoning: true, ToolCalling: true, ImageInput: true, Sampling: false}},
	{"openai", "gpt-5", ModelProfile{ContextWindow: 400000, MaxOutputTokens: 128000, Reasoning: true, ToolCalling: true, ImageInput: true, Sampling: false}},

	// OpenAI GPT-4 families — full sampling.
	{"openai", "gpt-4o-mini", ModelProfile{ContextWindow: 128000, MaxOutputTokens: 16384, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"openai", "gpt-4o", ModelProfile{ContextWindow: 128000, MaxOutputTokens: 16384, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"openai", "gpt-4-turbo", ModelProfile{ContextWindow: 128000, MaxOutputTokens: 4096, ToolCalling: true, ImageInput: true, Sampling: true}},
	{"openai", "gpt-4", ModelProfile{ContextWindow: 8192, MaxOutputTokens: 4096, ToolCalling: true, Sampling: true}},
	{"openai", "gpt-3.5", ModelProfile{ContextWindow: 16385, MaxOutputTokens: 4096, ToolCalling: true, Sampling: true}},

	// DeepSeek (OpenAI-compatible). deepseek-reasoner ignores temperature.
	{"openai", "deepseek-reasoner", ModelProfile{ContextWindow: 65536, MaxOutputTokens: 8192, Reasoning: true, ToolCalling: true, Sampling: false}},
	{"openai", "deepseek-chat", ModelProfile{ContextWindow: 65536, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},
	{"openai", "deepseek-v3", ModelProfile{ContextWindow: 65536, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},

	// Moonshot / Kimi (OpenAI-compatible).
	{"openai", "kimi-k2", ModelProfile{ContextWindow: 131072, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},
	{"openai", "moonshot-v1-8k", ModelProfile{ContextWindow: 8192, MaxOutputTokens: 4096, ToolCalling: true, Sampling: true}},
	{"openai", "moonshot-v1-32k", ModelProfile{ContextWindow: 32768, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},
	{"openai", "moonshot-v1-128k", ModelProfile{ContextWindow: 131072, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},

	// Qwen (OpenAI-compatible).
	{"openai", "qwen-turbo", ModelProfile{ContextWindow: 131072, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},
	{"openai", "qwen-plus", ModelProfile{ContextWindow: 131072, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},
	{"openai", "qwen-max", ModelProfile{ContextWindow: 32768, MaxOutputTokens: 8192, ToolCalling: true, Sampling: true}},
}

// LookupProfile returns the capability profile for a provider+model, matching
// the model by longest-known prefix (so dated variants resolve). ok=false for
// an unknown provider or model; callers must treat that as "no information"
// and keep their default behaviour.
func LookupProfile(providerName, model string) (ModelProfile, bool) {
	model = strings.ToLower(model)
	best := -1
	for i, e := range modelProfiles {
		if e.provider != providerName {
			continue
		}
		if strings.HasPrefix(model, e.prefix) &&
			(best < 0 || len(e.prefix) > len(modelProfiles[best].prefix)) {
			best = i
		}
	}
	if best < 0 {
		return ModelProfile{}, false
	}
	return modelProfiles[best].profile, true
}
