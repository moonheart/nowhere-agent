package openai

import (
	"os"
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

	var sawToolStart, sawToolStop bool
	var args string
	for _, e := range events {
		if e.Type == provider.EventBlockStart && e.Block != nil && e.Block.Type == provider.BlockToolUse {
			sawToolStart = true
			if e.Block.ToolUseID != "c1" || e.Block.ToolName != "read" {
				t.Errorf("tool block wrong: %+v", e.Block)
			}
		}
		if e.Type == provider.EventBlockStop && e.Index == toolBaseIndex {
			sawToolStop = true
		}
		if e.Type == provider.EventBlockDelta {
			args += e.Delta
		}
	}
	if !sawToolStart {
		t.Error("no tool_use block start")
	}
	// The tool_use block must be closed (block-stop) so the loop emits its
	// tool-use event; without it the tool result is orphaned on the client.
	if !sawToolStop {
		t.Error("no tool_use block stop — tool call left open at finish")
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

// TestDecoderFinishReasonLength verifies OpenAI's finish_reason "length" (a
// max_tokens truncation) surfaces as StopMaxTokens on an EventMessageStop, so
// the loop can tell it apart from a natural stop.
func TestDecoderFinishReasonLength(t *testing.T) {
	d := newStreamDecoder()
	var events []provider.Event
	for _, p := range []string{
		`{"choices":[{"delta":{"content":"partial"},"finish_reason":""}]}`,
		`{"choices":[{"delta":{"content":"..."},"finish_reason":"length"}]}`,
		`[DONE]`,
	} {
		events = append(events, d.feed([]byte(p))...)
	}
	var stop provider.StopReason
	for _, e := range events {
		if e.Type == provider.EventMessageStop && e.StopReason != provider.StopUnknown {
			stop = e.StopReason
		}
	}
	if stop != provider.StopMaxTokens {
		t.Errorf("stop = %q, want %q", stop, provider.StopMaxTokens)
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

// TestDecoderReasoningThenContent pins the reasoning-model wire format: the
// gateway streams chain-of-thought as reasoning_content, then the answer as
// content. Reasoning must become a thinking block that closes before the text
// block opens.
func TestDecoderReasoningThenContent(t *testing.T) {
	d := newStreamDecoder()
	var events []provider.Event
	for _, p := range []string{
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":"We"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":" think"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"Answer","role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":" here","role":"assistant"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	} {
		events = append(events, d.feed([]byte(p))...)
	}

	var thinking, text string
	var thinkingClosedBeforeText bool
	sawTextStart := false
	for _, e := range events {
		switch e.Type {
		case provider.EventBlockStart:
			if e.Block != nil && e.Block.Type == provider.BlockText {
				sawTextStart = true
			}
		case provider.EventBlockStop:
			if e.Index == thinkingIndex && !sawTextStart {
				thinkingClosedBeforeText = true
			}
		case provider.EventBlockDelta:
			switch e.Index {
			case thinkingIndex:
				thinking += e.Delta
			case textIndex:
				text += e.Delta
			}
		}
	}
	if thinking != "We think" {
		t.Errorf("thinking = %q", thinking)
	}
	if text != "Answer here" {
		t.Errorf("text = %q", text)
	}
	if !thinkingClosedBeforeText {
		t.Error("thinking block did not close before text block started")
	}
}

// TestRealDeepSeekSSEFixture feeds the captured raw SSE bytes from the live
// DeepSeek gateway through the full scanner, verifying the decoder handles the
// real wire format (reasoning_content + content + a final usage chunk).
func TestRealDeepSeekSSEFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/deepseek_reasoning.sse")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}

	out := make(chan provider.Event, 256)
	go streamEvents(readCloser{strings.NewReader(string(data))}, out)

	var thinking, text string
	var usage *provider.Usage
	var sawStart bool
	for ev := range out {
		switch ev.Type {
		case provider.EventMessageStart:
			sawStart = true
		case provider.EventBlockDelta:
			switch ev.Index {
			case thinkingIndex:
				thinking += ev.Delta
			case textIndex:
				text += ev.Delta
			}
		case provider.EventMessageStop:
			if ev.Usage != nil {
				usage = ev.Usage
			}
		case provider.EventError:
			t.Fatalf("decode error: %v", ev.Err)
		}
	}

	if !sawStart {
		t.Error("no message_start")
	}
	if thinking == "" {
		t.Error("expected reasoning_content to be decoded as thinking, got none")
	}
	if text != "nowhere raw sse" {
		t.Errorf("answer text = %q want %q", text, "nowhere raw sse")
	}
	if usage == nil || usage.OutputTokens != 24 {
		t.Errorf("usage = %+v", usage)
	}
}
