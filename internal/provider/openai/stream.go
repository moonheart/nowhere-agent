package openai

import (
	"encoding/json"

	"nowhere-agent/internal/provider"
)

// chunk is one OpenAI streaming SSE data payload.
type chunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
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
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// streamDecoder converts OpenAI's cumulative chunk stream into the canonical
// ordered event stream. It is stateful: it opens a block on first content and
// emits block-stop when finishing. Pure (fed decoded JSON, no I/O).
type streamDecoder struct {
	started     bool
	blockOpen   bool
	seenToolIDs map[int]string
}

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
		if ch.Delta.Content != "" {
			if !d.blockOpen {
				events = append(events, provider.Event{
					Type:  provider.EventBlockStart,
					Index: 0,
					Block: &provider.Block{Type: provider.BlockText},
				})
				d.blockOpen = true
			}
			events = append(events, provider.Event{
				Type:  provider.EventBlockDelta,
				Index: 0,
				Delta: ch.Delta.Content,
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if tc.ID != "" {
				d.seenToolIDs[tc.Index] = tc.ID
				events = append(events, provider.Event{
					Type:  provider.EventBlockStart,
					Index: tc.Index + 1,
					Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: tc.ID, ToolName: tc.Function.Name, ToolInput: map[string]any{}},
				})
			}
			if tc.Function.Arguments != "" {
				events = append(events, provider.Event{
					Type:  provider.EventBlockDelta,
					Index: tc.Index + 1,
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

// finish closes any open block and emits message-stop (idempotent).
func (d *streamDecoder) finish() []provider.Event {
	var events []provider.Event
	if d.blockOpen {
		events = append(events, provider.Event{Type: provider.EventBlockStop, Index: 0})
		d.blockOpen = false
	}
	return events
}
