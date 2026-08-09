package provider

import "testing"

func TestNormalizeBase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1"},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1"},
		{"https://api.anthropic.com/v1/messages", "https://api.anthropic.com/v1"},
		{"https://proxy.example.com/v1/models", "https://proxy.example.com/v1"},
		{"http://127.0.0.1:1234", "http://127.0.0.1:1234"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeBase(c.in); got != c.want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveEndpoint(t *testing.T) {
	cases := []struct {
		base, suffix, want string
	}{
		{"https://api.openai.com/v1", EndpointChat, "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", EndpointChat, "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/chat/completions", EndpointChat, "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1", EndpointModels, "https://api.openai.com/v1/models"},
		{"https://api.anthropic.com/v1", EndpointMsg, "https://api.anthropic.com/v1/messages"},
		{"http://127.0.0.1:1234", EndpointChat, "http://127.0.0.1:1234/chat/completions"},
	}
	for _, c := range cases {
		if got := ResolveEndpoint(c.base, c.suffix); got != c.want {
			t.Errorf("ResolveEndpoint(%q, %q) = %q, want %q", c.base, c.suffix, got, c.want)
		}
	}
}
