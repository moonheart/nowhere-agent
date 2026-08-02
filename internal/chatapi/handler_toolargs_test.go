package chatapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
)

// newTestEmitter builds an sseEmitter over a recorder for direct Emit-drive tests.
func newTestEmitter() (*sseEmitter, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	return &sseEmitter{w: rec, flusher: rec, msgID: "m1", textID: "t1", thinkID: "r1"}, rec
}

// TestEmitterStreamsToolArgsIncrementally pins the per-chunk flow: KindToolArgs
// opens the block once and forwards each fragment as a tool-call-delta, and the
// closing KindToolUse adds only tool-call-end — it must NOT re-start the block or
// re-send the full input (which would duplicate the args the deltas already
// delivered). The client reconstructs the input by concatenating deltas.
func TestEmitterStreamsToolArgsIncrementally(t *testing.T) {
	e, rec := newTestEmitter()
	ctx := context.Background()

	emit := func(kind agent.EventKind, payload any) {
		if err := e.Emit(ctx, kind, payload); err != nil {
			t.Fatalf("Emit %v: %v", kind, err)
		}
	}

	// Block opens (empty delta), then three argument fragments stream.
	emit(agent.KindToolArgs, map[string]any{"id": "tu1", "name": "write_file", "delta": ""})
	emit(agent.KindToolArgs, map[string]any{"id": "tu1", "name": "write_file", "delta": `{"content":"`})
	emit(agent.KindToolArgs, map[string]any{"id": "tu1", "name": "write_file", "delta": `# file`})
	emit(agent.KindToolArgs, map[string]any{"id": "tu1", "name": "write_file", "delta": `"}`})
	// Block stop: carries the full input, but it already streamed — close only.
	emit(agent.KindToolUse, map[string]any{"id": "tu1", "name": "write_file", "input": map[string]any{"content": "# file"}})

	out := rec.Body.String()

	if n := strings.Count(out, `"type":"tool-call-start"`); n != 1 {
		t.Errorf("tool-call-start count = %d, want 1 (no re-open on KindToolUse)\n---\n%s", n, out)
	}
	if n := strings.Count(out, `"type":"tool-call-delta"`); n != 3 {
		t.Errorf("tool-call-delta count = %d, want 3 (one per fragment, no full-input re-send)\n---\n%s", n, out)
	}
	if !strings.Contains(out, `"type":"tool-call-end"`) {
		t.Errorf("missing tool-call-end\n---\n%s", out)
	}
	// The full input must appear ONLY via the streamed fragments, never as one
	// marshalled blob — so the literal contiguous JSON must be absent.
	if strings.Contains(out, `{"content":"# file"}`) {
		t.Errorf("full input re-sent as one delta (duplicate)\n---\n%s", out)
	}
	// Order: start before any delta, deltas in order, end last. Anchor on the
	// argsText field values (the raw fragments collide with JSON escaping inside
	// the frame, so match the whole "argsText":"..." pair for each).
	ix := func(s string) int { return strings.Index(out, s) }
	start := ix(`"type":"tool-call-start"`)
	d1 := ix(`"argsText":"{\"content\":\""`)
	d2 := ix(`"argsText":"# file"`)
	d3 := ix(`"argsText":"\"}"`)
	end := ix(`"type":"tool-call-end"`)
	if !(start >= 0 && start < d1 && d1 < d2 && d2 < d3 && d3 < end) {
		t.Errorf("frames out of order: start=%d d1=%d d2=%d d3=%d end=%d\n---\n%s", start, d1, d2, d3, end, out)
	}
}

// TestEmitterToolUseFallbackFullArgs pins the non-streaming path: when no
// KindToolArgs deltas preceded it (the no-broker direct path, or a provider that
// emitted none), KindToolUse still emits the complete input as one tool-call-delta
// so the UI shows the arguments instead of "{}".
func TestEmitterToolUseFallbackFullArgs(t *testing.T) {
	e, rec := newTestEmitter()
	if err := e.Emit(context.Background(), agent.KindToolUse, map[string]any{
		"id": "tu1", "name": "write_file", "input": map[string]any{"path": "note.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{`"type":"tool-call-start"`, `"type":"tool-call-delta"`, `note.txt`, `"type":"tool-call-end"`} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback stream missing %q\n---\n%s", want, out)
		}
	}
	if n := strings.Count(out, `"type":"tool-call-delta"`); n != 1 {
		t.Errorf("tool-call-delta count = %d, want 1 (full input as a single delta)\n---\n%s", n, out)
	}
}
