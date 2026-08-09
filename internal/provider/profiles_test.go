package provider

import "testing"

func TestLookupProfileExactAndDated(t *testing.T) {
	p, ok := LookupProfile("anthropic", "claude-sonnet-4-20250514")
	if !ok {
		t.Fatal("dated claude-sonnet-4 variant should resolve")
	}
	if p.ContextWindow != 200000 || !p.ToolCalling || !p.Sampling {
		t.Errorf("profile wrong: %+v", p)
	}
}

// Longest-prefix wins: gpt-4o-mini must not resolve to the gpt-4o profile.
func TestLookupProfileLongestPrefix(t *testing.T) {
	p, ok := LookupProfile("openai", "gpt-4o-mini-2024-07-18")
	if !ok {
		t.Fatal("gpt-4o-mini should resolve")
	}
	if p.MaxOutputTokens != 16384 {
		t.Errorf("got gpt-4o profile? %+v", p)
	}
}

func TestLookupProfileReasoningModels(t *testing.T) {
	for _, model := range []string{"o3", "o3-mini", "o1", "gpt-5", "deepseek-reasoner"} {
		p, ok := LookupProfile("openai", model)
		if !ok {
			t.Errorf("%s should resolve", model)
			continue
		}
		if !p.Reasoning {
			t.Errorf("%s should be marked reasoning", model)
		}
		if p.Sampling {
			t.Errorf("%s must reject sampling params", model)
		}
	}
}

func TestLookupProfileNoToolCalling(t *testing.T) {
	p, ok := LookupProfile("openai", "o1-mini")
	if !ok {
		t.Fatal("o1-mini should resolve")
	}
	if p.ToolCalling {
		t.Error("o1-mini has no tool calling")
	}
}

func TestLookupProfileUnknown(t *testing.T) {
	if _, ok := LookupProfile("openai", "my-fine-tune-123"); ok {
		t.Error("unknown model must return ok=false (callers keep default behaviour)")
	}
	if _, ok := LookupProfile("anthropic", "gpt-4o"); ok {
		t.Error("cross-provider model must not resolve")
	}
	if _, ok := LookupProfile("bedrock", "claude-sonnet-4"); ok {
		t.Error("unknown provider must return ok=false")
	}
}
