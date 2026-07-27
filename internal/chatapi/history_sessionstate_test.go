package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
)

// TestHistoryEchoesSessionState pins O1 recovery: when a session has plan state
// persisted, GET /api/chat/history echoes it as the top-level sessionState field
// so the reloading client's plan panel is restored.
func TestHistoryEchoesSessionState(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(nil, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	user := identity.User{ID: "u1"}
	sess, err := rt.CreateSession(context.Background(), user.ID, "t")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := rt.SetSessionStateKV(context.Background(), sess.ID, "plan", json.RawMessage(`{"items":[{"content":"a","status":"completed"}]}`)); err != nil {
		t.Fatalf("SetSessionStateKV: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess.ID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("history status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"sessionState"`) || !strings.Contains(body, `"plan"`) {
		t.Errorf("history missing sessionState.plan:\n%s", body)
	}
}

// TestHistoryOmitsEmptySessionState verifies a session with no state emits no
// sessionState (null), not a stale/empty object.
func TestHistoryOmitsEmptySessionState(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(nil, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	user := identity.User{ID: "u1"}
	sess, err := rt.CreateSession(context.Background(), user.ID, "t")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess.ID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var out struct {
		SessionState map[string]json.RawMessage `json:"sessionState"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(out.SessionState) != 0 {
		t.Errorf("sessionState should be empty/null, got %v", out.SessionState)
	}
}
