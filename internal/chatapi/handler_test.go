package chatapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
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

func newTestLoop(ctx context.Context, system, model string) *agent.Loop {
	return agent.New(stubProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
}

// TestServeChatForwardsRequestedModel pins the chat-side model selection
// contract: the request's optional model field reaches the LoopFactory, and
// an absent field arrives as "" (the server resolves the default).
func TestServeChatForwardsRequestedModel(t *testing.T) {
	got := make(chan string, 1)
	loop := func(ctx context.Context, system, model string) *agent.Loop {
		got <- model
		return newTestLoop(ctx, system, model)
	}
	h := NewHandler(loop, "sys")
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4o-mini"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if m := <-got; m != "gpt-4o-mini" {
		t.Errorf("loop model = %q, want gpt-4o-mini", m)
	}

	req = httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if m := <-got; m != "" {
		t.Errorf("loop model without a request field = %q, want \"\"", m)
	}
}

// TestServeChatModels pins the model-picker contract: the lister's default +
// enabled names reach the client as JSON, a request without an authenticated
// user resolves with an empty user id (the lister still answers), an unwired
// lister serves an empty list, and a lister error 500s.
func TestServeChatModels(t *testing.T) {
	h := NewHandler(newTestLoop, "sys").WithModelLister(func(_ context.Context, userID string) (string, []string, error) {
		if userID == "nobody" {
			return "", nil, fmt.Errorf("no provider")
		}
		return "claude-sonnet-4-5", []string{"claude-sonnet-4-5", "claude-haiku-4-5"}, nil
	})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/chat/models", nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: "u1"}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Default string   `json:"default"`
		Models  []string `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Default != "claude-sonnet-4-5" || len(got.Models) != 2 || got.Models[1] != "claude-haiku-4-5" {
		t.Errorf("payload = %+v, want default + 2 models", got)
	}

	// An unauthenticated request carries no user: the lister still answers
	// (userID ""), so a missing identity never breaks the picker endpoint.
	req = httptest.NewRequest("GET", "/api/chat/models", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("anonymous status = %d body=%s", rec.Code, rec.Body.String())
	}

	// No lister wired: empty list, still 200.
	h2 := NewHandler(newTestLoop, "sys")
	mux2 := http.NewServeMux()
	h2.Register(mux2)
	rec = httptest.NewRecorder()
	mux2.ServeHTTP(rec, httptest.NewRequest("GET", "/api/chat/models", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) == "" {
		t.Fatalf("unwired lister status = %d body=%s", rec.Code, rec.Body.String())
	}

	// A lister error is an internal failure, not an empty picker.
	bad := NewHandler(newTestLoop, "sys").WithModelLister(func(context.Context, string) (string, []string, error) {
		return "", nil, fmt.Errorf("boom")
	})
	muxBad := http.NewServeMux()
	bad.Register(muxBad)
	rec = httptest.NewRecorder()
	muxBad.ServeHTTP(rec, httptest.NewRequest("GET", "/api/chat/models", nil))
	if rec.Code != 500 {
		t.Fatalf("lister error status = %d, want 500", rec.Code)
	}
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

func TestServeChatTruncatesSessionTitle(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore())
	h := NewHandler(newTestLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	// A long first user message must not become the full durable session title.
	long := strings.Repeat("长", maxSessionTitleRunes*2) + " END"
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, long)
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	page, err := rt.ListSessionsByUser(context.Background(), "", "", 10, nil)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(page.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(page.Sessions))
	}
	title := page.Sessions[0].Title
	if n := len([]rune(title)); n != maxSessionTitleRunes+1 {
		t.Errorf("title runes = %d, want %d (cap %d + ellipsis)", n, maxSessionTitleRunes+1, maxSessionTitleRunes)
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("title %q missing the truncation ellipsis", title)
	}
	if !strings.HasPrefix(title, long[:6]) {
		t.Errorf("title %q lost its prefix", title)
	}
}

func TestServeChatRejectsOversizedBody(t *testing.T) {
	h := NewHandler(newTestLoop, "sys")
	mux := http.NewServeMux()
	h.Register(mux)

	// A body over the 4 MiB chat bound must answer 413, not a truncated-json
	// 400 — an attacker cannot push unbounded bytes through the chat POST.
	big := strings.Repeat("x", 4<<20)
	body := `{"messages":[{"role":"user","content":"` + big + `"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s, want 413", rec.Code, rec.Body.String())
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
