package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// decodeUIMessageStream parses an SSE body into its chunk frames, mirroring the
// wire contract of assistant-stream's UIMessageStream decoder: frames are
// separated by a blank line, each is "data: <json>", and the stream must end
// with a mandatory "data: [DONE]" sentinel. It fails the test on any framing
// violation (a non-data line, an unparseable frame, or a missing [DONE]).
func decodeUIMessageStream(t *testing.T, body string) (chunks []map[string]any, sawDone bool) {
	t.Helper()
	body = strings.ReplaceAll(body, "\r\n", "\n")
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.Trim(frame, "\n")
		if frame == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("SSE frame does not start with 'data: ': %q", frame)
		}
		data := strings.TrimPrefix(frame, "data: ")
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("frame is not valid JSON: %q (%v)", data, err)
		}
		chunks = append(chunks, m)
	}
	return chunks, sawDone
}

// finishReasons is the strict FinishReason union from chunk-types.ts.
var finishReasons = map[string]bool{
	"stop": true, "length": true, "content-filter": true,
	"tool-calls": true, "error": true, "other": true, "unknown": true,
}

// validateChunk asserts one decoded chunk against the strict UIMessageStreamChunk
// union (chunk-types.ts): every emitted type must be a known spec type and carry
// its required fields. Types we never emit (source/file) are absent from the
// switch; anything emitted but unrecognised fails the conformance check.
func validateChunk(t *testing.T, c map[string]any) {
	t.Helper()
	typ, _ := c["type"].(string)
	require := func(field string) {
		t.Helper()
		if _, ok := c[field]; !ok {
			t.Errorf("chunk type %q missing required field %q: %v", typ, field, c)
		}
	}
	requireStr := func(field string) {
		t.Helper()
		if s, ok := c[field].(string); !ok || s == "" {
			t.Errorf("chunk type %q field %q must be a non-empty string: %v", typ, field, c)
		}
	}
	requireUsage := func(field string) {
		t.Helper()
		u, ok := c[field].(map[string]any)
		if !ok {
			t.Errorf("chunk type %q field %q must be a usage object: %v", typ, field, c)
			return
		}
		for _, k := range []string{"inputTokens", "outputTokens"} {
			if _, ok := u[k].(float64); !ok {
				t.Errorf("chunk type %q usage missing numeric %q: %v", typ, k, u)
			}
		}
	}

	switch {
	case typ == "start":
		requireStr("messageId")
	case typ == "text-start", typ == "reasoning-start":
		requireStr("id")
	case typ == "text-delta":
		if s, ok := c["textDelta"].(string); !ok {
			// assistant-stream tolerates the legacy "delta" key, but we emit the
			// strict "textDelta"; flag a regression to the legacy field.
			t.Errorf("text-delta must carry textDelta (got delta=%v): %v", c["delta"], c)
		} else if s == "" {
			t.Errorf("text-delta textDelta must be a string: %v", c)
		}
	case typ == "text-end", typ == "reasoning-end", typ == "tool-call-end":
		// No required fields beyond type.
	case typ == "reasoning-delta":
		require("delta")
	case typ == "tool-call-start":
		requireStr("id")
		requireStr("toolCallId")
		requireStr("toolName")
	case typ == "tool-call-delta":
		require("argsText")
	case typ == "tool-result":
		requireStr("toolCallId")
		require("result")
	case typ == "start-step":
		// messageId optional.
	case typ == "finish-step":
		if r, _ := c["finishReason"].(string); !finishReasons[r] {
			t.Errorf("finish-step finishReason %q not in union: %v", r, c)
		}
		requireUsage("usage")
		if _, ok := c["isContinued"].(bool); !ok {
			t.Errorf("finish-step missing bool isContinued: %v", c)
		}
	case typ == "finish":
		if r, _ := c["finishReason"].(string); !finishReasons[r] {
			t.Errorf("finish finishReason %q not in union: %v", r, c)
		}
		requireUsage("usage")
	case typ == "error":
		requireStr("errorText")
	case strings.HasPrefix(typ, "data-"):
		require("data")
	default:
		t.Errorf("emitted chunk type %q is not a known ui-message-stream type: %v", typ, c)
	}
}

// assertConformance runs the strict decoder + validator over an emitted stream
// body and asserts the framing invariants the client accumulator relies on: a
// mandatory [DONE] sentinel, exactly one "start" as the first chunk, and exactly
// one "finish" as the last content chunk.
func assertConformance(t *testing.T, body string) {
	t.Helper()
	chunks, sawDone := decodeUIMessageStream(t, body)
	if !sawDone {
		t.Errorf("stream missing mandatory [DONE] sentinel\n---\n%s", body)
	}
	if len(chunks) == 0 {
		t.Fatalf("stream emitted no chunks\n---\n%s", body)
	}
	for _, c := range chunks {
		validateChunk(t, c)
	}
	if chunks[0]["type"] != "start" {
		t.Errorf("first chunk must be start, got %v", chunks[0]["type"])
	}
	if chunks[len(chunks)-1]["type"] != "finish" {
		t.Errorf("last chunk must be finish, got %v", chunks[len(chunks)-1]["type"])
	}
	starts, finishes := 0, 0
	for _, c := range chunks {
		switch c["type"] {
		case "start":
			starts++
		case "finish":
			finishes++
		}
	}
	if starts != 1 || finishes != 1 {
		t.Errorf("expected exactly 1 start and 1 finish, got %d/%d", starts, finishes)
	}
}

// TestEmittedStreamConformsToUIMessageStreamSpec runs the strict spec validator
// over the two real emission paths. The direct path (no runtime) streams a text
// answer; the runtime path drives a tool-calling run so tool-call + step frames
// are emitted too. Every chunk on the wire must be a known spec type with its
// required fields, and the stream must open with start and close with
// finish+[DONE].
func TestEmittedStreamConformsToUIMessageStreamSpec(t *testing.T) {
	t.Run("direct text path", func(t *testing.T) {
		h := NewHandler(newTestLoop, "sys")
		mux := http.NewServeMux()
		h.Register(mux)
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		assertConformance(t, rec.Body.String())
	})

	t.Run("runtime tool path", func(t *testing.T) {
		p := &scriptToolProvider{}
		rt := session.NewRuntime(session.NewMemStore())
		h := NewHandler(toolLoop(p), "sys").WithRuntime(rt)
		mux := http.NewServeMux()
		h.Register(mux)
		owner := identity.User{ID: "owner"}

		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"go"}]}`))
		req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		assertConformance(t, rec.Body.String())

		// The tool path must have exercised the tool-call + step frames.
		body := rec.Body.String()
		for _, want := range []string{`"type":"tool-call-start"`, `"type":"finish-step"`, `"type":"tool-result"`} {
			if !strings.Contains(body, want) {
				t.Errorf("tool path missing %q\n---\n%s", want, body)
			}
		}
	})
}

// scriptToolProvider answers with one tool call, then a final text answer, so a
// runtime run exercises the multi-iteration tool path (tool-call frames +
// finish-step + tool-result + a continued step).
type scriptToolProvider struct {
	calls int
}

func (p *scriptToolProvider) Name() string { return "script-tool" }

func (p *scriptToolProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	p.calls++
	ch := make(chan provider.Event, 8)
	if p.calls == 1 {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "echo", ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{"x":1}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopToolUse, Usage: &provider.Usage{InputTokens: 5, OutputTokens: 3}}
	} else {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "done"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn, Usage: &provider.Usage{InputTokens: 8, OutputTokens: 2}}
	}
	close(ch)
	return ch, nil
}

// echoToolForConformance is a no-dependency read-only tool for the runtime path.
type echoToolForConformance struct{}

func (echoToolForConformance) Name() string           { return "echo" }
func (echoToolForConformance) Description() string    { return "echo" }
func (echoToolForConformance) Schema() map[string]any { return map[string]any{"type": "object"} }
func (echoToolForConformance) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (echoToolForConformance) Timeout() time.Duration { return time.Second }
func (echoToolForConformance) Call(_ context.Context, _ map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{Content: "ok"}, nil
}

func toolLoop(p *scriptToolProvider) LoopFactory {
	return func(ctx context.Context, system, model string) *agent.Loop {
		reg := toolruntime.NewRegistry()
		reg.Register(echoToolForConformance{})
		return agent.New(p, reg, agent.Config{Model: "m", System: system, MaxTokens: 100})
	}
}
