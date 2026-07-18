package chatapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/session"
)

// TestChatPersistsRunWithRuntime verifies that when a runtime is wired, a chat
// request starts a run, persists its events to the store, and completes it.
func TestChatPersistsRunWithRuntime(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newTestLoop, "sys").WithRuntime(rt)

	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	// A session should exist (created from the request).
	sessions := store.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	sess := sessions[0]

	// Its run should be complete (terminal), not still active.
	if _, ok, _ := rt.ActiveRun(req.Context(), sess.ID); ok {
		t.Error("run still active after request completed")
	}
	runs := store.RunsFor(sess.ID)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].Status != session.RunDone {
		t.Errorf("run status = %q want done", runs[0].Status)
	}

	// Events (text + done) should be persisted for replay.
	events := store.EventsFor(runs[0].ID)
	if len(events) == 0 {
		t.Fatal("expected persisted events for the run")
	}
	var sawText, sawDone bool
	for _, e := range events {
		switch e.Kind {
		case "text":
			sawText = true
		case "done":
			sawDone = true
		}
		if e.Offset == 0 {
			t.Errorf("event offset not assigned: %+v", e)
		}
	}
	if !sawText || !sawDone {
		t.Errorf("expected text and done events, got %+v", events)
	}
}

// TestChatResumesThreadSession verifies a request carrying a threadId reuses
// the existing session rather than creating a new one.
func TestChatResumesThreadSession(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newTestLoop, "sys").WithRuntime(rt)

	mux := http.NewServeMux()
	h.Register(mux)

	// First request creates the session.
	first := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	mux.ServeHTTP(httptest.NewRecorder(), first)
	if len(store.Sessions()) != 1 {
		t.Fatalf("expected 1 session after first request, got %d", len(store.Sessions()))
	}
	sessID := store.Sessions()[0].ID

	// Second request with the thread id resumes the same session.
	second := httptest.NewRequest("POST", "/api/chat",
		strings.NewReader(`{"threadId":"`+sessID+`","messages":[{"role":"user","content":"again"}]}`))
	mux.ServeHTTP(httptest.NewRecorder(), second)

	if len(store.Sessions()) != 1 {
		t.Errorf("thread resume created a new session; got %d", len(store.Sessions()))
	}
	if runs := store.RunsFor(sessID); len(runs) != 2 {
		t.Errorf("expected 2 runs on the resumed session, got %d", len(runs))
	}
}
