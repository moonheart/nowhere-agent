package dreaming

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

// scriptedAdapter is a provider.Adapter that replays a fixed event stream (or a
// start error), letting us drive ProviderLLM without a network.
type scriptedAdapter struct {
	events []provider.Event
	err    error
}

func (s *scriptedAdapter) Name() string { return "scripted" }

func (s *scriptedAdapter) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan provider.Event, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// TestProviderLLMAccumulatesTextAndTokens: Complete concatenates the text
// deltas and reports input+output tokens from the terminal usage event.
func TestProviderLLMAccumulatesTextAndTokens(t *testing.T) {
	a := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockDelta, Delta: "user "},
		{Type: provider.EventBlockDelta, Delta: "likes go"},
		{Type: provider.EventMessageStop, Usage: &provider.Usage{InputTokens: 30, OutputTokens: 12}},
	}}
	llm := NewProviderLLM(a, "test-model")

	text, tokens, err := llm.Complete(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "user likes go" {
		t.Errorf("text = %q", text)
	}
	if tokens != 42 {
		t.Errorf("tokens = %d want 42 (30 in + 12 out)", tokens)
	}
}

// TestProviderLLMErrorSurfaces: a stream EventError (or a start error) aborts
// the completion with that error.
func TestProviderLLMErrorSurfaces(t *testing.T) {
	boom := errors.New("boom")
	a := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockDelta, Delta: "partial"},
		{Type: provider.EventError, Err: boom},
	}}
	if _, _, err := NewProviderLLM(a, "m").Complete(context.Background(), "p"); !errors.Is(err, boom) {
		t.Errorf("stream error = %v want %v", err, boom)
	}

	startErr := &scriptedAdapter{err: boom}
	if _, _, err := NewProviderLLM(startErr, "m").Complete(context.Background(), "p"); !errors.Is(err, boom) {
		t.Errorf("start error = %v want %v", err, boom)
	}
}

// TestProviderLLMDropsThinking: reasoning models stream chain-of-thought on a
// thinking block (index 0) and the answer on a text block (index 1). Complete
// must return only the answer text — the chain-of-thought is not a fact and
// must never reach the memory store.
func TestProviderLLMDropsThinking(t *testing.T) {
	a := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockThinking}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "Let me analyze. "},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "Nothing durable here."},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventBlockStart, Index: 1, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 1, Delta: "user likes go"},
		{Type: provider.EventBlockStop, Index: 1},
		{Type: provider.EventMessageStop, Usage: &provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
	text, _, err := NewProviderLLM(a, "m").Complete(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if text != "user likes go" {
		t.Errorf("text = %q, want only the answer text (thinking dropped)", text)
	}
}

// TestProviderLLMCompleteJSON: a forced structured-output call accumulates the
// tool_use block's JSON deltas (ignoring any thinking/text prose) and
// unmarshals them into out. This is the L3 path that keeps chain-of-thought
// out of the memory store.
func TestProviderLLMCompleteJSON(t *testing.T) {
	a := &scriptedAdapter{events: []provider.Event{
		// Reasoning on the thinking block — must be ignored.
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockThinking}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "analyzing the transcript..."},
		{Type: provider.EventBlockStop, Index: 0},
		// The forced tool call carries the JSON payload.
		{Type: provider.EventBlockStart, Index: 1, Block: &provider.Block{Type: provider.BlockToolUse, ToolName: "record_facts"}},
		{Type: provider.EventBlockDelta, Index: 1, Delta: `{"facts":["user `},
		{Type: provider.EventBlockDelta, Index: 1, Delta: `likes go"]}`},
		{Type: provider.EventBlockStop, Index: 1},
		{Type: provider.EventMessageStop, Usage: &provider.Usage{InputTokens: 20, OutputTokens: 8}},
	}}
	llm := NewProviderLLM(a, "m")
	var res extractResult
	tokens, err := llm.CompleteJSON(context.Background(), "p", extractSchema, &res)
	if err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if tokens != 28 {
		t.Errorf("tokens = %d want 28", tokens)
	}
	if len(res.Facts) != 1 || res.Facts[0] != "user likes go" {
		t.Errorf("facts = %+v, want [user likes go] (thinking excluded)", res.Facts)
	}
}

// TestProviderLLMCompleteJSONErrors: no usable payload or malformed JSON is an error.
func TestProviderLLMCompleteJSONErrors(t *testing.T) {
	// Neither a tool_use block nor any text JSON.
	a := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockThinking}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "only reasoning, no answer"},
		{Type: provider.EventMessageStop},
	}}
	var res extractResult
	if _, err := NewProviderLLM(a, "m").CompleteJSON(context.Background(), "p", extractSchema, &res); err == nil {
		t.Error("expected error when model produces neither tool_use nor JSON text")
	}

	// Malformed JSON in the tool payload.
	b := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: `{not json`},
		{Type: provider.EventMessageStop},
	}}
	if _, err := NewProviderLLM(b, "m").CompleteJSON(context.Background(), "p", extractSchema, &res); err == nil {
		t.Error("expected decode error for malformed JSON")
	}
}

// TestProviderLLMCompleteJSONTextFallback: when the (soft-forced) model answers
// with prose JSON instead of a tool call, CompleteJSON extracts the object from
// the text block — and still ignores the reasoning block.
func TestProviderLLMCompleteJSONTextFallback(t *testing.T) {
	a := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockThinking}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: "the user has a cat, so facts are..."},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventBlockStart, Index: 1, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 1, Delta: "```json\n{\"facts\":[\"user has a cat named 豆豆\", \"user speaks chinese\"]}\n```"},
		{Type: provider.EventBlockStop, Index: 1},
		{Type: provider.EventMessageStop, Usage: &provider.Usage{InputTokens: 10, OutputTokens: 20}},
	}}
	var res extractResult
	tokens, err := NewProviderLLM(a, "m").CompleteJSON(context.Background(), "p", extractSchema, &res)
	if err != nil {
		t.Fatalf("CompleteJSON text fallback: %v", err)
	}
	if tokens != 30 {
		t.Errorf("tokens = %d want 30", tokens)
	}
	if len(res.Facts) != 2 || res.Facts[0] != "user has a cat named 豆豆" {
		t.Errorf("facts = %+v, want the 2 facts from the text JSON", res.Facts)
	}
}

// TestProviderLLMCompleteJSONTruncation: a stream that ends on max_tokens with
// a partial JSON payload reports a clear truncation error, not a raw decode
// failure.
func TestProviderLLMCompleteJSONTruncation(t *testing.T) {
	a := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: `{"add":[{"kind":"fact","content":"a long fact that got cut`},
		{Type: provider.EventMessageStop, StopReason: provider.StopMaxTokens, Usage: &provider.Usage{InputTokens: 5, OutputTokens: 4096}},
	}}
	var res consolidateResult
	_, err := NewProviderLLM(a, "m").CompleteJSON(context.Background(), "p", consolidateSchema, &res)
	if err == nil || !strings.Contains(err.Error(), "truncated at max_tokens") {
		t.Errorf("err = %v, want a max_tokens truncation error", err)
	}
}

// TestProviderLLMNoUsageDegradesToZero: a missing usage report degrades to 0
// tokens rather than failing the pass.
func TestProviderLLMNoUsageDegradesToZero(t *testing.T) {
	a := &scriptedAdapter{events: []provider.Event{
		{Type: provider.EventBlockDelta, Delta: "text"},
		{Type: provider.EventMessageStop},
	}}
	_, tokens, err := NewProviderLLM(a, "m").Complete(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Errorf("tokens = %d want 0 when usage is absent", tokens)
	}
}
