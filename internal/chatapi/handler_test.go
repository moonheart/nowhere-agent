package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// stubProvider streams a fixed text answer.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 5)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "Hi there"}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	close(ch)
	return ch, nil
}

func newTestLoop(ctx context.Context, system string) *agent.Loop {
	return agent.New(stubProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
}

func TestServeChatStreamsUIProtocol(t *testing.T) {
	h := NewHandler(newTestLoop, "sys")
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		`"type":"start"`,
		`"textDelta":"Hi there"`,
		`"type":"finish"`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("response missing %q\n---\n%s", want, out)
		}
	}
}

func TestToHistoryExtractsText(t *testing.T) {
	req := dataStreamRequest{
		Messages: []incomingMessage{
			{Role: "user", Content: json.RawMessage(`"first"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"reply"}]`)},
			{Role: "user", Parts: []incomingPart{{Type: "text", Text: "second"}}},
		},
	}
	h := toHistory(req)
	if len(h) != 3 {
		t.Fatalf("history len = %d", len(h))
	}
	if h[0].Content[0].Text != "first" {
		t.Errorf("h0 = %q", h[0].Content[0].Text)
	}
	if h[1].Role != provider.RoleAssistant || h[1].Content[0].Text != "reply" {
		t.Errorf("h1 = %+v", h[1])
	}
	if h[2].Content[0].Text != "second" {
		t.Errorf("h2 = %q", h[2].Content[0].Text)
	}
}

func TestLastUserText(t *testing.T) {
	req := dataStreamRequest{Messages: []incomingMessage{
		{Role: "user", Content: json.RawMessage(`"q1"`)},
		{Role: "assistant", Content: json.RawMessage(`"a1"`)},
		{Role: "user", Content: json.RawMessage(`"q2"`)},
	}}
	if got := lastUserText(req); got != "q2" {
		t.Errorf("lastUserText = %q", got)
	}
}
