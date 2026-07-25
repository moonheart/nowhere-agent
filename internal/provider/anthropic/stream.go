package anthropic

import (
	"encoding/json"

	"nowhere-agent/internal/provider"
)

// streamEvent is the raw Anthropic SSE event envelope.
type streamEvent struct {
	Type  string          `json:"type"`
	Index int             `json:"index,omitempty"`
	Block *rawBlock       `json:"content_block,omitempty"`
	Delta *rawDelta       `json:"delta,omitempty"`
	Usage *rawUsage       `json:"usage,omitempty"`
	Msg   *rawMessage     `json:"message,omitempty"`
}

type rawMessage struct {
	Usage *rawUsage `json:"usage,omitempty"`
}

type rawBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
}

type rawDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Signature   string `json:"signature,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type rawUsage struct {
	InputTokens          int `json:"input_tokens,omitempty"`
	OutputTokens         int `json:"output_tokens,omitempty"`
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationTokens  int `json:"cache_creation_input_tokens,omitempty"`
}

// decodeEvent parses one SSE data payload into a canonical provider.Event.
// Returns (event, true) if the payload maps to an event, or (zero, false) for
// payloads we intentionally ignore (e.g. ping, message_delta without usage).
func decodeEvent(data []byte) (provider.Event, bool) {
	var se streamEvent
	if err := json.Unmarshal(data, &se); err != nil {
		return provider.Event{Type: provider.EventError, Err: err}, true
	}

	switch se.Type {
	case "message_start":
		return provider.Event{Type: provider.EventMessageStart}, true

	case "content_block_start":
		if se.Block == nil {
			return provider.Event{}, false
		}
		return provider.Event{
			Type:  provider.EventBlockStart,
			Index: se.Index,
			Block: convertRawBlock(se.Block),
		}, true

	case "content_block_delta":
		if se.Delta == nil {
			return provider.Event{}, false
		}
		return provider.Event{
			Type:           provider.EventBlockDelta,
			Index:          se.Index,
			Delta:          deltaText(se.Delta),
			SignatureDelta: signatureDelta(se.Delta),
		}, true

	case "content_block_stop":
		return provider.Event{Type: provider.EventBlockStop, Index: se.Index}, true

	case "message_delta":
		// message_delta carries the final stop_reason (in delta) and usage. Emit an
		// EventMessageStop carrying whichever are present; drop only if neither is.
		ev := provider.Event{Type: provider.EventMessageStop}
		if se.Delta != nil {
			ev.StopReason = mapStopReason(se.Delta.StopReason)
		}
		if se.Usage != nil {
			ev.Usage = convertUsage(se.Usage)
		}
		if ev.StopReason == provider.StopUnknown && ev.Usage == nil {
			return provider.Event{}, false
		}
		return ev, true

	case "message_stop":
		return provider.Event{Type: provider.EventMessageStop}, true

	default:
		// ping, error envelopes handled elsewhere, unknown types ignored.
		return provider.Event{}, false
	}
}

func convertRawBlock(rb *rawBlock) *provider.Block {
	switch rb.Type {
	case "text":
		return &provider.Block{Type: provider.BlockText, Text: rb.Text}
	case "thinking":
		return &provider.Block{Type: provider.BlockThinking, Thinking: rb.Thinking, ThinkingSignature: rb.Signature}
	case "tool_use":
		return &provider.Block{Type: provider.BlockToolUse, ToolUseID: rb.ID, ToolName: rb.Name, ToolInput: map[string]any{}}
	default:
		return &provider.Block{Type: provider.BlockText}
	}
}

func deltaText(d *rawDelta) string {
	switch d.Type {
	case "text_delta":
		return d.Text
	case "thinking_delta":
		return d.Thinking
	case "input_json_delta":
		return d.PartialJSON
	default:
		return ""
	}
}

// signatureDelta extracts the signature fragment from a signature_delta,
// keeping it out of the thinking text stream.
func signatureDelta(d *rawDelta) string {
	if d.Type == "signature_delta" {
		return d.Signature
	}
	return ""
}

func convertUsage(u *rawUsage) *provider.Usage {
	return &provider.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationTokens,
	}
}

// mapStopReason maps Anthropic's stop_reason onto the neutral StopReason. Values
// without a neutral constant (pause_turn, refusal, …) pass through verbatim so
// the loop can still observe them; "" maps to StopUnknown.
func mapStopReason(s string) provider.StopReason {
	switch s {
	case "end_turn":
		return provider.StopEndTurn
	case "max_tokens":
		return provider.StopMaxTokens
	case "tool_use":
		return provider.StopToolUse
	case "stop_sequence":
		return provider.StopStopSequence
	default:
		return provider.StopReason(s)
	}
}
