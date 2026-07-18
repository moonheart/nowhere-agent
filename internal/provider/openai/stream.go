package openai

import (
	"encoding/json"

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
			ToolCalls        []struct {
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
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// streamDecoder converts OpenAI's cumulative chunk stream into the canonical
// ordered event stream. It is stateful: it opens a block on first content and
// emits block-stop when finishing. Reasoning (chain-of-thought) maps to a
// thinking block at index 0, answer text to a text block at index 1, and tool
// calls follow. Pure (fed decoded JSON, no I/O).
type streamDecoder struct {
	started      bool
	thinkingOpen bool
	textOpen     bool
	seenToolIDs  map[int]string
}

// Block indexes in the canonical stream.
const (
	thinkingIndex = 0
	textIndex     = 1
	toolBaseIndex = 2
)

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{seenToolIDs: map[int]string{}}
}

// feed parses one SSE data payload and returns the canonical events to emit.
func (d *streamDecoder) feed(data []byte) []provider.Event {
	if string(data) == "[DONE]" {
		return d.finish()
	}
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return []provider.Event{{Type: provider.EventError, Err: err}}
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
		if ch.Delta.ReasoningContent != "" {
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
				Delta: ch.Delta.ReasoningContent,
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
			if tc.ID != "" {
				d.seenToolIDs[tc.Index] = tc.ID
				events = append(events, provider.Event{
					Type:  provider.EventBlockStart,
					Index: tc.Index + toolBaseIndex,
					Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: tc.ID, ToolName: tc.Function.Name, ToolInput: map[string]any{}},
				})
			}
			if tc.Function.Arguments != "" {
				events = append(events, provider.Event{
					Type:  provider.EventBlockDelta,
					Index: tc.Index + toolBaseIndex,
					Delta: tc.Function.Arguments,
				})
			}
		}
		if ch.FinishReason != "" {
			events = append(events, d.finish()...)
		}
	}

	if c.Usage != nil {
		events = append(events, provider.Event{
			Type: provider.EventMessageStop,
			Usage: &provider.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
			},
		})
	}
	return events
}

// finish closes any open block (thinking then text) and is idempotent.
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
	return events
}
