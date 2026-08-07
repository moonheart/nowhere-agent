package chatapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// TestEmitterRendersStepFrames pins the wire shape of the step frames: start-step
// carries no messageId (the decoder falls back to the current message), and
// finish-step carries the step's reason, token usage, and isContinued flag — the
// fields the client accumulator reads to render per-step usage and continuation.
func TestEmitterRendersStepFrames(t *testing.T) {
	e, rec := newTestEmitter()
	ctx := context.Background()

	if err := e.Emit(ctx, agent.KindStepStart, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(ctx, agent.KindStepFinish, agent.StepEvent{
		FinishReason: "tool-calls",
		Usage:        &provider.Usage{InputTokens: 12, OutputTokens: 7},
		IsContinued:  true,
	}); err != nil {
		t.Fatal(err)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`"type":"start-step"`,
		`"type":"finish-step"`,
		`"finishReason":"tool-calls"`,
		`"inputTokens":12`,
		`"outputTokens":7`,
		`"isContinued":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("step frames missing %q\n---\n%s", want, body)
		}
	}
	// start-step must NOT carry a messageId (decoder supplies the current one).
	if strings.Contains(body, `"type":"start-step","messageId"`) {
		t.Errorf("start-step must omit messageId\n---\n%s", body)
	}
}

// TestEmitterStepFrameNilUsage pins that a step finishing without usage (a nil
// Usage pointer) still renders a well-formed finish-step frame with zeroed
// token counts rather than omitting the usage object the client expects.
func TestEmitterStepFrameNilUsage(t *testing.T) {
	e, rec := newTestEmitter()
	if err := e.Emit(context.Background(), agent.KindStepFinish, agent.StepEvent{FinishReason: "stop", Usage: nil, IsContinued: false}); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{`"type":"finish-step"`, `"finishReason":"stop"`, `"inputTokens":0`, `"outputTokens":0`, `"isContinued":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("nil-usage step frame missing %q\n---\n%s", want, body)
		}
	}
}

// TestEmitStreamEventStepFromBroker pins the live path: a StepEvent that crossed
// the StreamBroker as a JSON payload (snake_case keys, nested usage object) must
// decode via stepEvent and render the same finish-step frame as a direct Emit.
// This is the shape attached clients actually receive, so the field mapping is
// asserted here rather than only on the emitter.
func TestEmitStreamEventStepFromBroker(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "text-1", thinkID: "reasoning-1"}
	req := httptest.NewRequest("POST", "/api/chat/resume", nil)

	// start-step over the broker (nil payload).
	emitStreamEvent(req, emitter, session.StreamEvent{Kind: string(agent.KindStepStart)})

	// finish-step over the broker: payload is the JSON object the broker carried.
	payload, err := json.Marshal(map[string]any{
		"finish_reason": "stop",
		"is_continued":  false,
		"usage":         map[string]any{"input_tokens": 20, "output_tokens": 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	emitStreamEvent(req, emitter, session.StreamEvent{Kind: string(agent.KindStepFinish), Payload: payload})

	body := rec.Body.String()
	for _, want := range []string{`"type":"start-step"`, `"type":"finish-step"`, `"finishReason":"stop"`, `"inputTokens":20`, `"outputTokens":9`, `"isContinued":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("broker step frame missing %q\n---\n%s", want, body)
		}
	}
}

// TestStepEventToleratesMapAndStruct pins the payload-shape tolerance of the
// stepEvent decoder: it must accept the typed StepEvent, a pointer to it, and
// the JSON-decoded map (both snake_case and camelCase) that arrives over the
// broker, returning ok=false only for shapes it can't read.
func TestStepEventToleratesMapAndStruct(t *testing.T) {
	se := agent.StepEvent{FinishReason: "length", Usage: &provider.Usage{InputTokens: 1, OutputTokens: 2}, IsContinued: true}
	if got, ok := stepEvent(se); !ok || got.FinishReason != "length" || !got.IsContinued {
		t.Errorf("struct form: ok=%v got=%+v", ok, got)
	}
	if got, ok := stepEvent(&se); !ok || got.FinishReason != "length" {
		t.Errorf("pointer form: ok=%v got=%+v", ok, got)
	}
	snake := map[string]any{"finish_reason": "stop", "is_continued": false, "usage": map[string]any{"input_tokens": 3, "output_tokens": 4}}
	if got, ok := stepEvent(snake); !ok || got.FinishReason != "stop" || got.Usage == nil || got.Usage.InputTokens != 3 {
		t.Errorf("snake map form: ok=%v got=%+v", ok, got)
	}
	camel := map[string]any{"finishReason": "tool-calls", "isContinued": true, "usage": map[string]any{"inputTokens": 5, "outputTokens": 6}}
	if got, ok := stepEvent(camel); !ok || got.FinishReason != "tool-calls" || !got.IsContinued || got.Usage.OutputTokens != 6 {
		t.Errorf("camel map form: ok=%v got=%+v", ok, got)
	}
	if _, ok := stepEvent("not-a-step"); ok {
		t.Error("unexpected ok for non-step payload")
	}
	if _, ok := stepEvent(map[string]any{}); ok {
		t.Error("empty map (no reason) should not decode as a step")
	}
}
