package chatapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
)

// TestAttachStreamsSessionStateFrame is the end-to-end O1 live-push check: when
// a session_state frame is published to the broker mid-run, an attached client's
// SSE body carries a data-session-state frame. This pins the whole chain
// (broker → attach contentCh → emitStreamEvent → SSE), which is what the live
// plan panel depends on.
func TestAttachStreamsSessionStateFrame(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	p := newGatedProvider("hello")
	h := NewHandler(gatedLoop(p), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sessID, wait := startRunAsync(t, mux, p, owner, `{"messages":[{"role":"user","content":"go"}]}`)
	defer wait()

	// Attach a second client to the in-flight run.
	attached := make(chan string, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		attached <- rec.Body.String()
	}()

	// Let the attach subscribe, then publish a session_state frame the way
	// Runtime.SetSessionStateKV does, then release the run's content.
	time.Sleep(50 * time.Millisecond)
	if err := rt.SetSessionStateKV(context.Background(), sessID, "plan", []byte(`{"items":[{"content":"a","status":"in_progress"}]}`)); err != nil {
		t.Fatalf("SetSessionStateKV: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	close(p.gate)

	select {
	case body := <-attached:
		if !strings.Contains(body, `"type":"data-session-state"`) {
			t.Errorf("attached SSE body missing data-session-state frame\n---\n%s", body)
		}
		if !strings.Contains(body, `"key":"plan"`) {
			t.Errorf("attached SSE body missing plan key\n---\n%s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attached client did not receive the stream")
	}
}
