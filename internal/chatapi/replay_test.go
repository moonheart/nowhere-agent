package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// thinkingStub streams a reasoning block then a text answer.
type thinkingStub struct{}

func (thinkingStub) Name() string { return "thinking-stub" }
func (thinkingStub) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 10)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockThinking}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "pondering "}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "the cat"}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 1, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 1, Delta: "Doudou "}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 1, Delta: "is your cat"}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 1}
	ch <- provider.Event{Type: provider.EventMessageStop}
	close(ch)
	return ch, nil
}

func newThinkingLoop(ctx context.Context, system string) *agent.Loop {
	return agent.New(thinkingStub{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
}

// runChat posts one chat turn through the handler and returns the body.
func runChat(t *testing.T, mux *http.ServeMux, body string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("chat status = %d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestChatPersistsUserMessage verifies the user's text is written to the run
// log as the first event, so history replay can rebuild the user side.
func TestChatPersistsUserMessage(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	runChat(t, mux, `{"messages":[{"role":"user","content":"who is Doudou"}]}`)

	sess := store.Sessions()[0]
	runs := store.RunsFor(sess.ID)
	events := store.EventsFor(runs[0].ID)

	if len(events) == 0 || events[0].Kind != string(agent.KindUser) {
		t.Fatalf("first event should be the user message, got %+v", events)
	}
	var text string
	if err := json.Unmarshal(events[0].Payload, &text); err != nil || text != "who is Doudou" {
		t.Errorf("user payload = %q (err %v)", text, err)
	}
}

// TestChatEmitsSessionFrame verifies the stream carries a transient
// data-session frame naming the session, for the client to persist.
func TestChatEmitsSessionFrame(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	out := runChat(t, mux, `{"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(out, `"type":"data-session"`) {
		t.Errorf("stream missing data-session frame\n---\n%s", out)
	}
	if !strings.Contains(out, `"transient":true`) {
		t.Errorf("data-session frame should be transient\n---\n%s", out)
	}
	sessID := store.Sessions()[0].ID
	if !strings.Contains(out, `"id":"`+sessID+`"`) {
		t.Errorf("data-session should name session %s\n---\n%s", sessID, out)
	}
}

// TestHistoryRebuildsConversation runs two turns on one session and verifies
// GET /history rebuilds user + assistant (reasoning+text) messages in order.
func TestHistoryRebuildsConversation(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	// First turn creates the session.
	runChat(t, mux, `{"messages":[{"role":"user","content":"q1"}]}`)
	sessID := store.Sessions()[0].ID
	// Second turn resumes it.
	runChat(t, mux, `{"threadId":"`+sessID+`","messages":[{"role":"user","content":"q2"}]}`)

	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sessID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("history status = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Messages []historyMessage `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(resp.Messages) != 4 {
		t.Fatalf("expected 4 messages (2 user + 2 assistant), got %d: %+v", len(resp.Messages), resp.Messages)
	}

	// Turn 1.
	if resp.Messages[0].Role != "user" || resp.Messages[0].Content[0].Text != "q1" {
		t.Errorf("msg0 = %+v", resp.Messages[0])
	}
	a1 := resp.Messages[1]
	if a1.Role != "assistant" || len(a1.Content) != 2 {
		t.Fatalf("assistant msg should have reasoning+text parts: %+v", a1)
	}
	if a1.Content[0].Type != "reasoning" || a1.Content[0].Text != "pondering the cat" {
		t.Errorf("reasoning part = %+v", a1.Content[0])
	}
	if a1.Content[1].Type != "text" || a1.Content[1].Text != "Doudou is your cat" {
		t.Errorf("text part = %+v", a1.Content[1])
	}

	// Turn 2 resumed the same session.
	if resp.Messages[2].Role != "user" || resp.Messages[2].Content[0].Text != "q2" {
		t.Errorf("msg2 = %+v", resp.Messages[2])
	}
	if resp.Messages[3].Role != "assistant" {
		t.Errorf("msg3 = %+v", resp.Messages[3])
	}
}

// TestResumeReplaysRun verifies POST /resume streams the run's persisted events
// after the offset as ui-message-stream frames.
func TestResumeReplaysRun(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	runChat(t, mux, `{"messages":[{"role":"user","content":"q1"}]}`)
	sessID := store.Sessions()[0].ID

	req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("resume status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		`"type":"reasoning-start"`,
		`"delta":"pondering "`,
		`"type":"text-start"`,
		`"delta":"Doudou "`,
		`"type":"finish"`,
		"data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("resume stream missing %q\n---\n%s", want, out)
		}
	}
}

// TestResumeHonorsOffset verifies after=N skips already-seen events.
func TestResumeHonorsOffset(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	runChat(t, mux, `{"messages":[{"role":"user","content":"q1"}]}`)
	sessID := store.Sessions()[0].ID
	runID := store.RunsFor(sessID)[0].ID
	total := len(store.EventsFor(runID))

	// Replay from the final offset: nothing new should stream.
	req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after="+strconv.Itoa(total), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	out := rec.Body.String()
	if strings.Contains(out, `"delta":"Doudou "`) {
		t.Errorf("after=final offset should stream no deltas\n---\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("resume should still terminate\n---\n%s", out)
	}
}

// TestHistoryForbiddenForForeignSession verifies a caller cannot read another
// user's session history (finding #5).
func TestHistoryForbiddenForForeignSession(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	// Session owned by user A (created via an authenticated request).
	owner := identity.User{ID: "user-a"}
	createReq := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"secret"}]}`))
	createReq = createReq.WithContext(identity.NewContextWithUser(createReq.Context(), owner))
	mux.ServeHTTP(httptest.NewRecorder(), createReq)
	sessID := store.Sessions()[0].ID

	// user B tries to read it.
	intruder := identity.User{ID: "user-b"}
	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sessID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), intruder))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign history status = %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestResumeForbiddenForForeignSession verifies resume also enforces ownership.
func TestResumeForbiddenForForeignSession(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	owner := identity.User{ID: "user-a"}
	createReq := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"secret"}]}`))
	createReq = createReq.WithContext(identity.NewContextWithUser(createReq.Context(), owner))
	mux.ServeHTTP(httptest.NewRecorder(), createReq)
	sessID := store.Sessions()[0].ID

	intruder := identity.User{ID: "user-b"}
	req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), intruder))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign resume status = %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestChatDoesNotResumeForeignSession verifies a threadId pointing at another
// user's session is not resumed; a fresh session is created instead (#5).
func TestChatDoesNotResumeForeignSession(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	owner := identity.User{ID: "user-a"}
	createReq := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"secret"}]}`))
	createReq = createReq.WithContext(identity.NewContextWithUser(createReq.Context(), owner))
	mux.ServeHTTP(httptest.NewRecorder(), createReq)
	sessA := store.Sessions()[0].ID

	// user B sends a chat naming user A's session as threadId.
	intruder := identity.User{ID: "user-b"}
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"threadId":"`+sessA+`","messages":[{"role":"user","content":"hijack"}]}`))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), intruder))
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if len(store.Sessions()) != 2 {
		t.Fatalf("expected a fresh session for the foreign threadId, got %d sessions", len(store.Sessions()))
	}
	for _, s := range store.Sessions() {
		if s.ID == sessA {
			if runs := store.RunsFor(sessA); len(runs) != 1 {
				t.Errorf("foreign session A gained a run: %d", len(runs))
			}
		}
	}
}

// TestSessionsListsOnlyCallersSessions verifies GET /sessions returns the
// authenticated user's sessions and no one else's.
func TestSessionsListsOnlyCallersSessions(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	// user A has two sessions, user B has one.
	alice := identity.User{ID: "alice"}
	for _, q := range []string{"a1", "a2"} {
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"`+q+`"}]}`))
		req = req.WithContext(identity.NewContextWithUser(req.Context(), alice))
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	bob := identity.User{ID: "bob"}
	breq := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"b1"}]}`))
	breq = breq.WithContext(identity.NewContextWithUser(breq.Context(), bob))
	mux.ServeHTTP(httptest.NewRecorder(), breq)

	// Alice lists hers.
	req := httptest.NewRequest("GET", "/api/chat/sessions", nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), alice))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("sessions status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sessions []sessionDTO `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(resp.Sessions) != 2 {
		t.Fatalf("alice should see 2 sessions, got %d: %+v", len(resp.Sessions), resp.Sessions)
	}

	// Unauthenticated request is rejected.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/chat/sessions", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated sessions status = %d want 401", rec2.Code)
	}
}

// TestDeleteSession verifies a user can delete their own session (it leaves the
// list) and cannot delete someone else's (404, indistinguishable).
func TestDeleteSession(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	alice := identity.User{ID: "alice"}
	mkChat := func(u identity.User, q string) {
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"`+q+`"}]}`))
		req = req.WithContext(identity.NewContextWithUser(req.Context(), u))
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	mkChat(alice, "alice chat")
	bob := identity.User{ID: "bob"}
	mkChat(bob, "bob chat")

	sessions := store.Sessions()
	var aliceSess, bobSess string
	for _, s := range sessions {
		if s.UserID == "alice" {
			aliceSess = s.ID
		} else {
			bobSess = s.ID
		}
	}

	// Alice cannot delete Bob's session (404).
	req := httptest.NewRequest("DELETE", "/api/chat/sessions/"+bobSess, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), alice))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("alice deleting bob's session = %d want 404", rec.Code)
	}

	// Alice deletes her own (204) and it leaves her list.
	req2 := httptest.NewRequest("DELETE", "/api/chat/sessions/"+aliceSess, nil)
	req2 = req2.WithContext(identity.NewContextWithUser(req2.Context(), alice))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Errorf("alice deleting own session = %d want 204", rec2.Code)
	}
	remaining, _ := store.ListSessionsByUser(req2.Context(), "alice")
	if len(remaining) != 0 {
		t.Errorf("deleted session still listed: %+v", remaining)
	}
}
