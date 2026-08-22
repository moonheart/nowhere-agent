package openai

import (
	"encoding/json"
	"fmt"
	"sort"

	"nowhere-agent/internal/provider"
)

// chunk is one OpenAI streaming SSE data payload.
type chunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// ReasoningContent carries chain-of-thought for reasoning models
			// (e.g. DeepSeek); it streams separately from content.
			ReasoningContent string `json:"reasoning_content"`
			// Reasoning is the same chain-of-thought under its other common
			// gateway field name; some OpenAI-compatible gateways send
			// "reasoning" instead of "reasoning_content". Either name is
			// accepted.
			Reasoning string `json:"reasoning"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
	// Err carries a mid-stream error payload. Some OpenAI-compatible gateways
	// report failures (including context-length rejections) as an SSE data
	// frame after the 200 OK instead of a non-200 status.
	Err *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// usage is the OpenAI streaming usage payload. PromptCacheHitTokens is
// DeepSeek's automatic prefix-cache hit count; PromptTokensDetails carries
// OpenAI's official cached-token count. Either may be absent.
type usage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// CompletionTokensDetails carries the reasoning-token share of the
	// completion (reasoning models). Absent on non-reasoning models.
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// reasoning returns the reasoning-token share of the completion, 0 when the
// provider does not break it out.
func (u *usage) reasoning() int {
	if u.CompletionTokensDetails != nil {
		return u.CompletionTokensDetails.ReasoningTokens
	}
	return 0
}

// cacheRead picks the cache-hit count: DeepSeek's prompt_cache_hit_tokens,
// falling back to OpenAI's prompt_tokens_details.cached_tokens.
func (u *usage) cacheRead() int {
	if u.PromptCacheHitTokens != 0 {
		return u.PromptCacheHitTokens
	}
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

// streamDecoder converts OpenAI's cumulative chunk stream into the canonical
// ordered event stream. It is stateful: it opens a block on first content and
// emits block-stop when finishing. Reasoning (chain-of-thought) maps to a
// thinking block at index 0, answer text to a text block at index 1, and tool
// calls follow. Pure (fed decoded JSON, no I/O).
type streamDecoder struct {
	started             bool
	thinkingOpen        bool
	textOpen            bool
	seenToolIDs         map[int]string // canonical tool index -> id
	providerToCanonical map[int]int    // provider tool_calls[].index -> canonical
	nextToolIndex       int
	finishEmitted       bool
}

// Block indexes in the canonical stream.
const (
	thinkingIndex = 0
	textIndex     = 1
	toolBaseIndex = 2
)

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{seenToolIDs: map[int]string{}, providerToCanonical: map[int]int{}}
}

// feed parses one SSE data payload and returns the canonical events to emit.
func (d *streamDecoder) feed(data []byte) []provider.Event {
	if string(data) == "[DONE]" {
		hasTools := len(d.seenToolIDs) > 0
		hasContent := d.thinkingOpen || d.textOpen || hasTools
		evs := d.finish()
		if !d.finishEmitted && hasContent {
			reason := provider.StopEndTurn
			if hasTools {
				reason = provider.StopToolUse
			}
			evs = append(evs, provider.Event{Type: provider.EventMessageStop, StopReason: reason})
		}
		return evs
	}
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return []provider.Event{{Type: provider.EventError, Err: err}}
	}

	// A mid-stream error frame (gateway-reported failure after the 200 OK).
	// Classify it so a context-length rejection surfaces as a
	// ContextOverflowError and the loop's overflow fallback can retry with a
	// shrunk view instead of failing the run.
	if c.Err != nil {
		if ov := provider.ClassifyStreamError(c.Err.Message); ov != nil {
			return []provider.Event{{Type: provider.EventError, Err: ov}}
		}
		return []provider.Event{{Type: provider.EventError, Err: fmt.Errorf("openai stream error (%s/%s): %s", c.Err.Type, c.Err.Code, c.Err.Message)}}
	}

	var events []provider.Event
	if !d.started {
		events = append(events, provider.Event{Type: provider.EventMessageStart})
		d.started = true
	}

	if len(c.Choices) > 0 {
		ch := c.Choices[0]

		// Reasoning (chain-of-thought) → thinking block. Reasoning always
		// precedes answer text; when text begins, the thinking block closes.
		// Gateways name the field "reasoning_content" or "reasoning"; either
		// is decoded.
		reasoning := ch.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = ch.Delta.Reasoning
		}
		if reasoning != "" {
			if !d.thinkingOpen {
				events = append(events, provider.Event{
					Type:  provider.EventBlockStart,
					Index: thinkingIndex,
					Block: &provider.Block{Type: provider.BlockThinking},
				})
				d.thinkingOpen = true
			}
			events = append(events, provider.Event{
				Type:  provider.EventBlockDelta,
				Index: thinkingIndex,
				Delta: reasoning,
			})
		}

		if ch.Delta.Content != "" {
			if d.thinkingOpen {
				events = append(events, provider.Event{Type: provider.EventBlockStop, Index: thinkingIndex})
				d.thinkingOpen = false
			}
			if !d.textOpen {
				events = append(events, provider.Event{
					Type:  provider.EventBlockStart,
					Index: textIndex,
					Block: &provider.Block{Type: provider.BlockText},
				})
				d.textOpen = true
			}
			events = append(events, provider.Event{
				Type:  provider.EventBlockDelta,
				Index: textIndex,
				Delta: ch.Delta.Content,
			})
		}

		for _, tc := range ch.Delta.ToolCalls {
			pIdx := tc.Index
			canonical := -1
			if tc.ID != "" {
				if existing, ok := d.providerToCanonical[pIdx]; ok {
					if d.seenToolIDs[existing] == tc.ID {
						canonical = existing
					} else {
						// Different id at same provider index: proxy collapsed
						// parallel calls onto index 0. Allocate a new canonical
						// index so the second call doesn't collide at
						// toolBaseIndex+pIdx and hard-fail the loop.
						canonical = d.nextToolIndex
						d.nextToolIndex++
						d.providerToCanonical[pIdx] = canonical
						d.seenToolIDs[canonical] = tc.ID
						events = append(events, provider.Event{
							Type:  provider.EventBlockStart,
							Index: canonical + toolBaseIndex,
							Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: tc.ID, ToolName: tc.Function.Name, ToolInput: map[string]any{}},
						})
					}
				} else {
					canonical = d.nextToolIndex
					d.nextToolIndex++
					d.providerToCanonical[pIdx] = canonical
					d.seenToolIDs[canonical] = tc.ID
					events = append(events, provider.Event{
						Type:  provider.EventBlockStart,
						Index: canonical + toolBaseIndex,
						Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: tc.ID, ToolName: tc.Function.Name, ToolInput: map[string]any{}},
					})
				}
			} else if tc.Function.Arguments != "" {
				if c, ok := d.providerToCanonical[pIdx]; ok {
					canonical = c
				} else {
					// Arguments without a prior start: fallback to pIdx
					canonical = pIdx
				}
			}
			if tc.Function.Arguments != "" {
				if canonical == -1 {
					if c, ok := d.providerToCanonical[pIdx]; ok {
						canonical = c
					} else {
						canonical = pIdx
					}
				}
				events = append(events, provider.Event{
					Type:  provider.EventBlockDelta,
					Index: canonical + toolBaseIndex,
					Delta: tc.Function.Arguments,
				})
			}
		}
		if ch.FinishReason != "" {
			events = append(events, d.finish()...)
			d.finishEmitted = true
			// Surface why generation ended (finish_reason) so the loop can tell a
			// natural stop from a "length" truncation. Usage arrives in a later
			// chunk (include_usage) as its own EventMessageStop; the loop merges.
			events = append(events, provider.Event{
				Type:       provider.EventMessageStop,
				StopReason: mapFinishReason(ch.FinishReason),
			})
		}
	}

	if c.Usage != nil {
		events = append(events, provider.Event{
			Type: provider.EventMessageStop,
			Usage: &provider.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
				// There is no explicit cache-write on OpenAI/DeepSeek (the prefix
				// cache is automatic), so CacheWriteTokens stays 0.
				CacheReadTokens: c.Usage.cacheRead(),
				ReasoningTokens: c.Usage.reasoning(),
			},
		})
	}
	return events
}

// finish closes any open block (thinking, text, and tool calls) and is
// idempotent. OpenAI streams tool calls with no explicit per-call terminator —
// they stay open until finish_reason/[DONE] — so tool blocks must be closed
// here too. Without their block-stop the loop finalizes them only at stream
// close and never emits the tool-use event, orphaning the tool result on the
// client (which then rejects a result for an unknown tool-call id).
func (d *streamDecoder) finish() []provider.Event {
	var events []provider.Event
	if d.thinkingOpen {
		events = append(events, provider.Event{Type: provider.EventBlockStop, Index: thinkingIndex})
		d.thinkingOpen = false
	}
	if d.textOpen {
		events = append(events, provider.Event{Type: provider.EventBlockStop, Index: textIndex})
		d.textOpen = false
	}
	if len(d.seenToolIDs) > 0 {
		idxs := make([]int, 0, len(d.seenToolIDs))
		for i := range d.seenToolIDs {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			events = append(events, provider.Event{Type: provider.EventBlockStop, Index: i + toolBaseIndex})
		}
		d.seenToolIDs = map[int]string{}
	}
	return events
}

// mapFinishReason maps OpenAI's finish_reason onto the neutral StopReason.
// Unknown values (content_filter, …) pass through verbatim so the loop can still
// observe them; "" maps to StopUnknown.
func mapFinishReason(s string) provider.StopReason {
	switch s {
	case "stop":
		return provider.StopEndTurn
	case "length":
		return provider.StopMaxTokens
	case "tool_calls", "function_call":
		return provider.StopToolUse
	default:
		return provider.StopReason(s)
	}
}
