package dreaming

import (
	"context"
	"strings"

	"nowhere-agent/internal/provider"
)

// ProviderLLM adapts a provider.Adapter to the worker's LLM interface
// (capability-gap K1). It reuses the loop's adapter and model but sends NO
// tools — the same shape as contextmgmt.LLMCompressor — so a dreaming call can
// only emit text. Each Complete drains the stream and reports the tokens the
// terminal usage event consumed, feeding the worker's budget accounting.
type ProviderLLM struct {
	adapter provider.Adapter
	model   string
	// MaxTokens bounds a single completion (extractions are small). Zero uses a
	// default.
	MaxTokens int
}

// NewProviderLLM creates an LLM over the given adapter and model.
func NewProviderLLM(adapter provider.Adapter, model string) *ProviderLLM {
	return &ProviderLLM{adapter: adapter, model: model, MaxTokens: 1024}
}

// Complete runs one no-tools generation and returns its text plus the tokens it
// consumed (input + output). A missing usage report degrades to 0 tokens rather
// than failing the pass — the budget then simply under-counts for that call.
func (l *ProviderLLM) Complete(ctx context.Context, prompt string) (string, int, error) {
	req := provider.Request{
		Model:     l.model,
		Messages:  []provider.Message{provider.TextMessage(provider.RoleUser, prompt)},
		Tools:     nil, // no tools: the worker only wants prose back
		MaxTokens: l.MaxTokens,
	}
	events, err := l.adapter.Stream(ctx, req)
	if err != nil {
		return "", 0, err
	}
	var sb strings.Builder
	tokens := 0
	for ev := range events {
		switch ev.Type {
		case provider.EventBlockDelta:
			sb.WriteString(ev.Delta)
		case provider.EventMessageStop:
			if ev.Usage != nil {
				tokens = ev.Usage.InputTokens + ev.Usage.OutputTokens
			}
		case provider.EventError:
			return "", tokens, ev.Err
		}
	}
	return sb.String(), tokens, nil
}
