package agent

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/redact"
	"nowhere-agent/internal/toolruntime"
)

// TestRedactMWScrubsToolResult: the middleware replaces sensitive content in a
// tool's result — success and error results alike (an error string can quote
// the very secret it failed on) — while leaving the rest of the result intact.
func TestRedactMWScrubsToolResult(t *testing.T) {
	r, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	mw := &RedactMW{Redactor: r}

	cases := []struct {
		name            string
		in              string
		wantPlaceholder bool
	}{
		{"success", "logged in as alice@example.com", true},
		{"error", "failed: token sk-ant-abcdefghijklmnopqrstuvwxyz123456789 rejected", true},
		{"clean", "all 137 tests passed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var nextCalls int
			res := mw.WrapToolCall(context.Background(), &ToolCall{
				Call: toolruntime.Call{Name: "run_command"},
			}, func(_ context.Context, _ *ToolCall) toolruntime.Result {
				nextCalls++
				return toolruntime.Result{Content: c.in}
			})
			if nextCalls != 1 {
				t.Fatalf("WrapToolCall must delegate to next, got %d calls", nextCalls)
			}
			if strings.Contains(res.Content, "alice@example.com") || strings.Contains(res.Content, "sk-ant-") {
				t.Errorf("secret leaked through: %q", res.Content)
			}
			if got := strings.Contains(res.Content, "REDACTED_"); got != c.wantPlaceholder {
				t.Errorf("placeholder presence = %v, want %v (result %q)", got, c.wantPlaceholder, res.Content)
			}
		})
	}
}

// TestRedactMWScrubsNestedBlocks: the sub-conversation blocks a spawn_agent
// result carries are scrubbed too — text, thinking, and nested tool content,
// recursing through nested trees.
func TestRedactMWScrubsNestedBlocks(t *testing.T) {
	r, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	mw := &RedactMW{Redactor: r}

	secret := "alice@example.com"
	blocks := []provider.Block{
		{Type: provider.BlockText, Text: "the caller is " + secret},
		{Type: provider.BlockThinking, Thinking: "note: " + secret},
		{Type: provider.BlockToolResult, ToolContent: "reply to " + secret,
			ToolMessages: []provider.Block{
				{Type: provider.BlockText, Text: "deeper: " + secret},
				{Type: provider.BlockToolResult, ToolContent: secret},
			}},
	}

	out := mw.WrapToolCall(context.Background(), &ToolCall{}, func(_ context.Context, _ *ToolCall) toolruntime.Result {
		return toolruntime.Result{Content: "", Nested: blocks}
	}).Nested

	var walk func([]provider.Block)
	walk = func(bs []provider.Block) {
		for _, b := range bs {
			if strings.Contains(b.Text, secret) || strings.Contains(b.Thinking, secret) || strings.Contains(b.ToolContent, secret) {
				t.Errorf("secret survived in nested block %+v", b)
			}
			walk(b.ToolMessages)
		}
	}
	walk(out)
}

// TestRedactMWNilRedactorPassThrough: a middleware with a nil Redactor (wired
// when redaction is disabled) is a transparent passthrough.
func TestRedactMWNilRedactorPassThrough(t *testing.T) {
	mw := &RedactMW{} // Redactor nil
	in := "hello alice@example.com"
	res := mw.WrapToolCall(context.Background(), &ToolCall{}, func(_ context.Context, _ *ToolCall) toolruntime.Result {
		return toolruntime.Result{Content: in}
	})
	if res.Content != in {
		t.Errorf("nil redactor must pass through, got %q", res.Content)
	}
}
