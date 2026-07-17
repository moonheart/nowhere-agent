package openai

import (
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

func TestDecoderTextStream(t *testing.T) {
	d := newStreamDecoder()
	var events []provider.Event
	for _, payload := range []string{
		`{"choices":[{"delta":{"content":"Hel"},"finish_reason":""}]}`,
		`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	} {
		events = append(events, d.feed([]byte(payload))...)
	}

	var types []provider.EventType
	var text string
	for _, e := range events {
		types = append(types, e.Type)
		if e.Type == provider.EventBlockDelta {
			text += e.Delta
		}
	}
	if text != "Hello" {
		t.Errorf("reassembled text = %q", text)
	}
	if types[0] != provider.EventMessageStart {
		t.Errorf("first event = %q", types[0])
	}
}

func TestDecoderToolCall(t *testing.T) {
	d := newStreamDecoder()
	var events []provider.Event
	payloads := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":""}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]},"finish_reason":""}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th}"}}]},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	for _, p := range payloads {
		events = append(events, d.feed([]byte(p))...)
	}

	var sawToolStart bool
	var args string
	for _, e := range events {
		if e.Type == provider.EventBlockStart && e.Block != nil && e.Block.Type == provider.BlockToolUse {
			sawToolStart = true
			if e.Block.ToolUseID != "c1" || e.Block.ToolName != "read" {
				t.Errorf("tool block wrong: %+v", e.Block)
			}
		}
		if e.Type == provider.EventBlockDelta {
			args += e.Delta
		}
	}
	if !sawToolStart {
		t.Error("no tool_use block start")
	}
	if args != `{"path}` {
		t.Errorf("tool args = %q", args)
	}
}

func TestDecoderUsage(t *testing.T) {
	d := newStreamDecoder()
	events := d.feed([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`))
	var usage *provider.Usage
	for _, e := range events {
		if e.Usage != nil {
			usage = e.Usage
		}
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 4 {
		t.Fatalf("usage wrong: %+v", usage)
	}
}

func TestDecoderInvalidJSON(t *testing.T) {
	d := newStreamDecoder()
	events := d.feed([]byte(`{bad`))
	if len(events) == 0 || events[len(events)-1].Type != provider.EventError {
		t.Errorf("expected error event, got %+v", events)
	}
}

// TestStreamEventsEndToEnd feeds a raw SSE body through the scanner.
func TestStreamEventsEndToEnd(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":""}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out := make(chan provider.Event, 16)
	go streamEvents(readCloser{strings.NewReader(sse)}, out)

	var text string
	var usage *provider.Usage
	var sawStart bool
	for ev := range out {
		switch ev.Type {
		case provider.EventMessageStart:
			sawStart = true
		case provider.EventBlockDelta:
			text += ev.Delta
		case provider.EventMessageStop:
			if ev.Usage != nil {
				usage = ev.Usage
			}
		}
	}
	if !sawStart {
		t.Error("no message_start")
	}
	if text != "Hi!" {
		t.Errorf("text = %q", text)
	}
	if usage == nil || usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", usage)
	}
}

type readCloser struct{ *strings.Reader }

func (r readCloser) Close() error { return nil }
