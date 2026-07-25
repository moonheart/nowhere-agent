package anthropic

import (
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

func decode(t *testing.T, payload string) provider.Event {
	t.Helper()
	ev, ok := decodeEvent([]byte(payload))
	if !ok {
		t.Fatalf("expected event for payload %s", payload)
	}
	return ev
}

func TestDecodeMessageStart(t *testing.T) {
	ev := decode(t, `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`)
	if ev.Type != provider.EventMessageStart {
		t.Errorf("got %q", ev.Type)
	}
}

func TestDecodeTextBlockStart(t *testing.T) {
	ev := decode(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	if ev.Type != provider.EventBlockStart || ev.Index != 0 {
		t.Fatalf("unexpected: %+v", ev)
	}
	if ev.Block == nil || ev.Block.Type != provider.BlockText {
		t.Errorf("block not converted: %+v", ev.Block)
	}
}

func TestDecodeToolUseBlockStart(t *testing.T) {
	ev := decode(t, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"read"}}`)
	if ev.Block == nil || ev.Block.Type != provider.BlockToolUse {
		t.Fatalf("unexpected block: %+v", ev.Block)
	}
	if ev.Block.ToolUseID != "tu1" || ev.Block.ToolName != "read" {
		t.Errorf("tool fields wrong: %+v", ev.Block)
	}
}

func TestDecodeThinkingBlockStart(t *testing.T) {
	ev := decode(t, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
	if ev.Block.Type != provider.BlockThinking {
		t.Errorf("got %q", ev.Block.Type)
	}
}

func TestDecodeTextDelta(t *testing.T) {
	ev := decode(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
	if ev.Type != provider.EventBlockDelta || ev.Delta != "Hello" {
		t.Errorf("unexpected: %+v", ev)
	}
}

func TestDecodeThinkingDelta(t *testing.T) {
	ev := decode(t, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`)
	if ev.Delta != "hmm" {
		t.Errorf("got delta %q", ev.Delta)
	}
}

func TestDecodeInputJSONDelta(t *testing.T) {
	ev := decode(t, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}`)
	if ev.Delta != `{"pa` {
		t.Errorf("got delta %q", ev.Delta)
	}
}

// TestDecodeSignatureDelta verifies the signature streams on its own channel,
// not folded into Delta (so it never lands in the thinking text).
func TestDecodeSignatureDelta(t *testing.T) {
	ev := decode(t, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`)
	if ev.Type != provider.EventBlockDelta {
		t.Fatalf("got %q", ev.Type)
	}
	if ev.SignatureDelta != "sig-abc" {
		t.Errorf("SignatureDelta = %q, want sig-abc", ev.SignatureDelta)
	}
	if ev.Delta != "" {
		t.Errorf("Delta = %q, want empty (signature must not enter text)", ev.Delta)
	}
}

func TestDecodeBlockStop(t *testing.T) {
	ev := decode(t, `{"type":"content_block_stop","index":2}`)
	if ev.Type != provider.EventBlockStop || ev.Index != 2 {
		t.Errorf("unexpected: %+v", ev)
	}
}

func TestDecodeMessageDeltaUsage(t *testing.T) {
	ev := decode(t, `{"type":"message_delta","usage":{"input_tokens":100,"output_tokens":42,"cache_read_input_tokens":80,"cache_creation_input_tokens":5}}`)
	if ev.Type != provider.EventMessageStop {
		t.Fatalf("got %q", ev.Type)
	}
	if ev.Usage == nil {
		t.Fatal("expected usage")
	}
	if ev.Usage.OutputTokens != 42 || ev.Usage.CacheReadTokens != 80 || ev.Usage.CacheWriteTokens != 5 {
		t.Errorf("usage wrong: %+v", ev.Usage)
	}
}

// TestDecodeMessageDeltaStopReason verifies the stop_reason on message_delta is
// mapped to the neutral StopReason so a max_tokens truncation is visible to the
// loop (it previously only read usage from message_delta and dropped the reason).
func TestDecodeMessageDeltaStopReason(t *testing.T) {
	ev := decode(t, `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":50}}`)
	if ev.Type != provider.EventMessageStop {
		t.Fatalf("got %q", ev.Type)
	}
	if ev.StopReason != provider.StopMaxTokens {
		t.Errorf("StopReason = %q, want %q", ev.StopReason, provider.StopMaxTokens)
	}
}

func TestDecodePingIgnored(t *testing.T) {
	if _, ok := decodeEvent([]byte(`{"type":"ping"}`)); ok {
		t.Error("ping should be ignored")
	}
}

func TestDecodeInvalidJSON(t *testing.T) {
	ev, ok := decodeEvent([]byte(`{not json`))
	if !ok || ev.Type != provider.EventError {
		t.Errorf("expected error event, got %+v ok=%v", ev, ok)
	}
}

// TestStreamEventsEndToEnd feeds a raw SSE body through the scanner and
// verifies the ordered canonical events that come out.
func TestStreamEventsEndToEnd(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
		``,
	}, "\n")

	out := make(chan provider.Event, 16)
	go streamEvents(readCloser{strings.NewReader(sse)}, out)

	var got []provider.EventType
	for ev := range out {
		got = append(got, ev.Type)
	}

	want := []provider.EventType{
		provider.EventMessageStart,
		provider.EventBlockStart,
		provider.EventBlockDelta,
		provider.EventBlockStop,
		provider.EventMessageStop,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: got %q want %q", i, got[i], want[i])
		}
	}
}

type readCloser struct{ *strings.Reader }

func (r readCloser) Close() error { return nil }
