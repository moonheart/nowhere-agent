package dreaming

import (
	"context"
	"errors"
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
