package chatapi

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
)

// The three tests below pin the frame shapes the production sseEmitter emits
// (the former streamWriter, removed: its finish() hardcoded finishReason "stop"
// and zero usage while the emitter latches the real terminal reason, so frames
// pinned through streamWriter could drift from the wire). The start frame is
// written the same way the production call sites do (handler.go), and the
// finish frame's usage defaults to 0/0 when no KindUsage preceded — exactly the
// values streamWriter pinned.

func TestEmitterTextFramesShape(t *testing.T) {
	e, rec := newTestEmitter()
	ctx := context.Background()
	e.write(chunk{"type": "start", "messageId": e.msgID})
	for _, d := range []string{"Hello", " world"} {
		if err := e.Emit(ctx, agent.KindText, d); err != nil {
			t.Fatalf("Emit text: %v", err)
		}
	}
	e.finish()

	out := rec.Body.String()

	mustContain := []string{
		`data: {"messageId":"m1","type":"start"}`,
		`"type":"text-start"`,
		`"textDelta":"Hello"`,
		`"type":"text-end"`,
		`"type":"finish"`,
		"data: [DONE]",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestEmitterToolCallFramesShape(t *testing.T) {
	e, rec := newTestEmitter()
	ctx := context.Background()
	emit := func(kind agent.EventKind, payload any) {
		t.Helper()
		if err := e.Emit(ctx, kind, payload); err != nil {
			t.Fatalf("Emit %v: %v", kind, err)
		}
	}
	emit(agent.KindToolUse, map[string]any{"id": "tc1", "name": "read"})
	emit(agent.KindToolResult, map[string]any{"tool_use_id": "tc1", "content": "file contents", "is_error": false})
	out := rec.Body.String()
	for _, want := range []string{`"type":"tool-call-start"`, `"toolCallId":"tc1"`, `"type":"tool-result"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestEmitterErrorFrame(t *testing.T) {
	e, rec := newTestEmitter()
	if err := e.Emit(context.Background(), agent.KindError, "something broke"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"errorText":"something broke"`) {
		t.Errorf("error chunk missing: %s", rec.Body.String())
	}
}
