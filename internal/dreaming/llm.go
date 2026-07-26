package dreaming

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Track which block indexes are answer text vs. thinking. Reasoning models
	// (DeepSeek etc.) stream chain-of-thought as EventBlockDelta on a thinking
	// block AND the answer on a text block, both via the same event type. Only
	// the text block is the completion — the chain-of-thought must NOT be
	// accumulated, or the worker would store the model's inner monologue as
	// "facts". Deltas carry no block type, so we learn each index's kind from its
	// BlockStart; a delta with no seen start is assumed text (test/simple
	// adapters emit bare deltas).
	textIdx := map[int]bool{}
	seenStart := map[int]bool{}
	for ev := range events {
		switch ev.Type {
		case provider.EventBlockStart:
			seenStart[ev.Index] = true
			textIdx[ev.Index] = ev.Block == nil || ev.Block.Type == provider.BlockText
		case provider.EventBlockDelta:
			if !seenStart[ev.Index] || textIdx[ev.Index] {
				sb.WriteString(ev.Delta)
			}
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

// CompleteJSON runs one STRUCTURED generation (capability L3): it forces the
// model to emit a single JSON object conforming to spec.Schema via a forced
// tool call, and returns that object unmarshalled into out (a pointer). The
// forced tool call structurally excludes reasoning/commentary — the answer is a
// tool_use block's JSON input, not free text — so this is immune to the
// chain-of-thought-leaks-into-facts bug that plain text parsing suffered.
//
// The tool_use input streams as JSON deltas on the tool block; CompleteJSON
// accumulates those deltas (tracked by the block index whose BlockStart carried
// a tool_use) and unmarshals them at stream end. Returns the tokens consumed.
func (l *ProviderLLM) CompleteJSON(ctx context.Context, prompt string, spec *provider.JSONResponseSpec, out any) (int, error) {
	req := provider.Request{
		Model:        l.model,
		Messages:     []provider.Message{provider.TextMessage(provider.RoleUser, prompt)},
		MaxTokens:    l.MaxTokens,
		JSONResponse: spec,
	}
	events, err := l.adapter.Stream(ctx, req)
	if err != nil {
		return 0, err
	}
	var jsonBuf strings.Builder
	tokens := 0
	toolIdx := map[int]bool{}
	for ev := range events {
		switch ev.Type {
		case provider.EventBlockStart:
			if ev.Block != nil && ev.Block.Type == provider.BlockToolUse {
				toolIdx[ev.Index] = true
			}
		case provider.EventBlockDelta:
			if toolIdx[ev.Index] {
				jsonBuf.WriteString(ev.Delta)
			}
		case provider.EventMessageStop:
			if ev.Usage != nil {
				tokens = ev.Usage.InputTokens + ev.Usage.OutputTokens
			}
		case provider.EventError:
			return tokens, ev.Err
		}
	}
	raw := strings.TrimSpace(jsonBuf.String())
	if raw == "" {
		return tokens, fmt.Errorf("structured output: model produced no tool_use payload")
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return tokens, fmt.Errorf("structured output: decode %q: %w", raw, err)
	}
	return tokens, nil
}
