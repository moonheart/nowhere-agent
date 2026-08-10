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

// Domestic (Chinese) model lines resolve with sane windows and flags — the
// deployment path for 国内企业 via OpenAI-compatible gateways.
func TestLookupProfileDomesticModels(t *testing.T) {
	cases := []struct {
		model      string
		window     int
		reasoning  bool
		sampling   bool
	}{
		{"deepseek-v3.1", 131072, true, true},
		{"deepseek-r1", 131072, true, false},
		{"glm-4.5-air", 204800, true, true},
		{"glm-4-plus", 131072, false, true},
		{"qwen3-max", 131072, true, true},
		{"qwen3-235b-a22b", 131072, true, true},
		{"qwen-turbo-latest", 131072, false, true},
		{"kimi-k2.5", 262144, false, true},
		{"doubao-seed-1.6", 262144, true, true},
		{"ernie-4.5-turbo-128k", 131072, false, true},
		{"minimax-M1", 262144, false, true},
		{"baichuan4", 131072, false, true},
	}
	for _, c := range cases {
		p, ok := LookupProfile("openai", c.model)
		if !ok {
			t.Errorf("%s should resolve", c.model)
			continue
		}
		if p.ContextWindow != c.window {
			t.Errorf("%s: window = %d, want %d", c.model, p.ContextWindow, c.window)
		}
		if p.Reasoning != c.reasoning {
			t.Errorf("%s: reasoning = %v, want %v", c.model, p.Reasoning, c.reasoning)
		}
		if p.Sampling != c.sampling {
			t.Errorf("%s: sampling = %v, want %v", c.model, p.Sampling, c.sampling)
		}
		if !p.ToolCalling {
			t.Errorf("%s: should support tool calling", c.model)
		}
	}
}

// Prefix specificity: a newer domestic line must not fall back to the family
// entry ("deepseek-v3" must not swallow "deepseek-v3.1").
func TestLookupProfileDomesticLongestPrefix(t *testing.T) {
	p, ok := LookupProfile("openai", "deepseek-v3.2-0606")
	if !ok {
		t.Fatal("deepseek-v3.2 should resolve")
	}
	if p.ContextWindow != 131072 {
		t.Errorf("deepseek-v3.2 resolved with window %d, want 131072 (not the v3 entry)", p.ContextWindow)
	}
}
