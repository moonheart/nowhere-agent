package chatapi

import (
	"net/http/httptest"
	"strings"
	"testing"

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
