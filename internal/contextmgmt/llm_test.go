package contextmgmt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

// fakeAdapter records the request and replays canned stream events.
type fakeAdapter struct {
	gotReq  provider.Request
	events  []provider.Event
	err     error
	sawCtx  context.Context
	streamN int
}

func (f *fakeAdapter) Name() string { return "fake" }

func (f *fakeAdapter) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	f.streamN++
	f.gotReq = req
	f.sawCtx = ctx
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan provider.Event, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// textStream builds a block delta stream carrying the given text.
func textStream(text string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: text},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop},
	}
}

func TestLLMCompressorSendsNoTools(t *testing.T) {
	fa := &fakeAdapter{events: textStream("<summary>the gist</summary>")}
	c := NewLLMCompressor(fa, "test-model")

	dropped := []provider.Message{
		provider.TextMessage(provider.RoleUser, "build a web app"),
		provider.TextMessage(provider.RoleAssistant, "sure, using Go"),
	}
	summary, err := c.Summarize(context.Background(), dropped)
	if err != nil {
		t.Fatal(err)
	}
	if fa.gotReq.Tools != nil {
		t.Error("summarizer must send no tools")
	}
	if fa.gotReq.Model != "test-model" {
		t.Errorf("model = %q, want test-model", fa.gotReq.Model)
	}
	if summary != "the gist" {
		t.Errorf("summary = %q, want unwrapped body", summary)
	}
}

func TestLLMCompressorRendersAllBlockKinds(t *testing.T) {
	fa := &fakeAdapter{events: textStream("s")}
	c := NewLLMCompressor(fa, "m")
	dropped := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Block{
			{Type: provider.BlockThinking, Thinking: "hmm"},
			{Type: provider.BlockText, Text: "checking"},
			{Type: provider.BlockToolUse, ToolUseID: "t1", ToolName: "search"},
		}},
		{Role: provider.RoleUser, Content: []provider.Block{
			{Type: provider.BlockToolResult, ToolResultID: "t1", ToolContent: "found it"},
		}},
	}
	if _, err := c.Summarize(context.Background(), dropped); err != nil {
		t.Fatal(err)
	}
	prompt := fa.gotReq.Messages[0].Content[0].Text
	for _, want := range []string{"hmm", "checking", "search", "found it"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("transcript missing %q:\n%s", want, prompt)
		}
	}
}

func TestLLMCompressorStripsAnalysis(t *testing.T) {
	fa := &fakeAdapter{events: textStream("<analysis>scratch</analysis><summary>real</summary>")}
	c := NewLLMCompressor(fa, "m")
	summary, err := c.Summarize(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "x")})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "real" {
		t.Errorf("summary = %q, want analysis stripped", summary)
	}
}

func TestLLMCompressorPropagatesError(t *testing.T) {
	fa := &fakeAdapter{err: errors.New("adapter down")}
	c := NewLLMCompressor(fa, "m")
	if _, err := c.Summarize(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "x")}); err == nil {
		t.Error("expected adapter error to propagate")
	}
}

func TestLLMCompressorStreamError(t *testing.T) {
	fa := &fakeAdapter{events: []provider.Event{
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventError, Err: errors.New("mid-stream boom")},
	}}
	c := NewLLMCompressor(fa, "m")
	if _, err := c.Summarize(context.Background(), []provider.Message{provider.TextMessage(provider.RoleUser, "x")}); err == nil {
		t.Error("expected mid-stream error to propagate")
	}
}
