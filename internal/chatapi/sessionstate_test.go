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

// TestEmitStreamEventSessionState pins O1: a session_state broker frame (the
// live push from Runtime.SetSessionStateKV) renders as a non-transient
// data-session-state SSE frame, so attached clients update the plan panel live
// and the frame survives in message metadata for a reload.
func TestEmitStreamEventSessionState(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}
	r := httptest.NewRequest("GET", "/", nil)

	payload := []byte(`{"key":"plan","value":{"items":[{"content":"Read config","status":"in_progress"}]}}`)
	emitStreamEvent(r, e, session.StreamEvent{Kind: "session_state", Payload: payload})

	body := rec.Body.String()
	for _, want := range []string{`"type":"data-session-state"`, `"key":"plan"`, `"content":"Read config"`} {
		if !strings.Contains(body, want) {
			t.Errorf("session-state frame missing %s:\n%s", want, body)
		}
	}
	// Non-transient: a transient frame would carry "transient":true and be
	// dropped on reload instead of landing in message metadata.
	if strings.Contains(body, `"transient":true`) {
		t.Errorf("session-state frame must be non-transient:\n%s", body)
	}
}

// TestServeSetSessionStateWritesPermissionMode pins the client state endpoint:
// an owner can write the allow-listed permission_mode key, which lands in the
// session's state store (read back via SessionStateKV).
func TestServeSetSessionStateWritesPermissionMode(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore())
	h := NewHandler(nil, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sess, err := rt.CreateSession(context.Background(), owner.ID, "t")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/chat/sessions/"+sess.ID+"/state",
		strings.NewReader(`{"key":"permission_mode","value":"allow_all"}`))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set state status = %d, body %s", rec.Code, rec.Body.String())
	}

	v, ok, err := rt.SessionStateKV(context.Background(), sess.ID, PermissionModeStateKey)
	if err != nil || !ok {
		t.Fatalf("permission_mode not stored (ok=%v, err=%v)", ok, err)
	}
	var mode string
	if err := json.Unmarshal(v, &mode); err != nil || mode != PermissionModeAllowAll {
		t.Errorf("stored mode = %q (err %v), want %q", mode, err, PermissionModeAllowAll)
	}
}

// TestServeSetSessionStateRejectsNonAllowListedKey pins that the generic state
// store (shared with the model's plan scratchpad) is not open to arbitrary client
// writes: a non-allow-listed key is forbidden.
func TestServeSetSessionStateRejectsNonAllowListedKey(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore())
	h := NewHandler(nil, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sess, err := rt.CreateSession(context.Background(), owner.ID, "t")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/chat/sessions/"+sess.ID+"/state",
		strings.NewReader(`{"key":"plan","value":{}}`))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-allow-listed key status = %d, want 403", rec.Code)
	}
}

// TestServeSetSessionStateRequiresOwnership pins that one user cannot write
// another user's session state.
func TestServeSetSessionStateRequiresOwnership(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore())
	h := NewHandler(nil, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	sess, err := rt.CreateSession(context.Background(), "owner", "t")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/chat/sessions/"+sess.ID+"/state",
		strings.NewReader(`{"key":"permission_mode","value":"allow_all"}`))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: "intruder"}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("a non-owner must not write the session's state")
	}
}
