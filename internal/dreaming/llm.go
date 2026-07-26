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

// NewProviderLLM creates an LLM over the given adapter and model. MaxTokens
// bounds a single completion; it must be generous because (a) reasoning models
// burn reasoning_tokens that count toward the cap and (b) a reflect/compress
// JSON payload can be long — a too-small cap truncates the JSON mid-stream and
// fails the decode.
func NewProviderLLM(adapter provider.Adapter, model string) *ProviderLLM {
	return &ProviderLLM{adapter: adapter, model: model, MaxTokens: 4096}
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

// CompleteJSON runs one STRUCTURED generation (capability L3): it asks the
// model to emit a single JSON object conforming to spec.Schema, and returns
// that object unmarshalled into out (a pointer).
//
// The preferred path is a (soft-forced) tool call: the answer arrives as a
// tool_use block's JSON input, structurally separated from any reasoning prose.
// But the OpenAI-compatible gateway rejects a forced tool_choice, so we only
// INSTRUCT the model to call the tool — and a reasoning model occasionally
// answers with prose instead. So CompleteJSON also captures the text block and,
// when there is no tool payload, falls back to extracting the JSON object from
// the text (which, per the prompt, is the only thing the text should contain).
// Reasoning/thinking blocks are always ignored either way, so the
// chain-of-thought can never be parsed as data.
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
	var jsonBuf, textBuf strings.Builder
	tokens := 0
	toolIdx := map[int]bool{}
	textIdx := map[int]bool{}
	truncated := false
	for ev := range events {
		switch ev.Type {
		case provider.EventBlockStart:
			if ev.Block != nil {
				switch ev.Block.Type {
				case provider.BlockToolUse:
					toolIdx[ev.Index] = true
				case provider.BlockText:
					textIdx[ev.Index] = true
				}
			}
		case provider.EventBlockDelta:
			if toolIdx[ev.Index] {
				jsonBuf.WriteString(ev.Delta)
			} else if textIdx[ev.Index] {
				textBuf.WriteString(ev.Delta)
			}
		case provider.EventMessageStop:
			if ev.Usage != nil {
				tokens = ev.Usage.InputTokens + ev.Usage.OutputTokens
			}
			if ev.StopReason == provider.StopMaxTokens {
				truncated = true
			}
		case provider.EventError:
			return tokens, ev.Err
		}
	}

	raw := strings.TrimSpace(jsonBuf.String())
	if raw == "" {
		// Soft-forcing fell through to prose: extract the JSON object from the
		// text block (the prompt told the model to put only the JSON there).
		raw = extractJSONObject(textBuf.String())
	}
	if raw == "" {
		return tokens, fmt.Errorf("structured output: model produced neither a tool_use payload nor JSON text")
	}
	if truncated {
		return tokens, fmt.Errorf("structured output: truncated at max_tokens (%d), JSON incomplete: %s", l.MaxTokens, truncate(raw, 120))
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return tokens, fmt.Errorf("structured output: decode %q: %w", truncate(raw, 200), err)
	}
	return tokens, nil
}

// extractJSONObject pulls the first balanced {...} object out of s, tolerating
// markdown code fences and surrounding prose. Returns "" if none is found.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	// Strip a leading code fence (```json ... ```) if present.
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return "" // unbalanced — likely truncated
}

// truncate shortens s for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
