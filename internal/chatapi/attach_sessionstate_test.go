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
	cb := newCountingBroker(session.NewMemBroker(0))
	rt := session.NewRuntime(store).WithBroker(cb)
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
	// Runtime.SetSessionStateKV does, then release the run's content. The
	// publish is synchronous (the broker fans the frame into the subscriber's
	// channel before returning), so once the subscription is registered there
	// is nothing further to wait for before releasing the gate. Subscribers
	// include the submitter, so two means the resumer has attached.
	waitFor(t, func() bool { return cb.subscribers(sessID) >= 2 }, "attach never subscribed to the broker")
	if err := rt.SetSessionStateKV(context.Background(), sessID, "plan", []byte(`{"items":[{"content":"a","status":"in_progress"}]}`)); err != nil {
		t.Fatalf("SetSessionStateKV: %v", err)
	}
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

// TestAttachStreamsSessionScopedStateFrame pins the empty-RunID contract: a
// session_state write made OUTSIDE any run carries an empty RunID (session-
// scoped) and must still reach an attach to a later run — the plan panel stays
// live across runs instead of silently dropping the out-of-band write.
func TestAttachStreamsSessionScopedStateFrame(t *testing.T) {
	store := session.NewMemStore()
	cb := newCountingBroker(session.NewMemBroker(0))
	rt := session.NewRuntime(store).WithBroker(cb)
	p := newGatedProvider("hello")
	h := NewHandler(gatedLoop(p), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	// Run 1: settle it fully so no run is active.
	sessID, wait := startRunAsync(t, mux, p, owner, `{"messages":[{"role":"user","content":"go"}]}`)
	close(p.gate)
	wait()

	// Out-of-band state write: no active run, so the frame carries an empty
	// RunID (session-scoped). Published after run 1 settled, it survives the
	// settle's ring cleanup.
	if err := rt.SetSessionStateKV(context.Background(), sessID, "plan", []byte(`{"items":[{"content":"a","status":"completed"}]}`)); err != nil {
		t.Fatalf("SetSessionStateKV: %v", err)
	}

	// Run 2 in the SAME session, held in flight (re-arm the provider gate),
	// then attach: the session-scoped frame must arrive with run 2's stream.
	p.mu.Lock()
	p.gate = make(chan struct{})
	p.mu.Unlock()
	sessID2, wait2 := startRunAsync(t, mux, p, owner, `{"messages":[{"role":"user","content":"go again"}],"threadId":"`+sessID+`"}`)
	defer wait2()
	if sessID2 != sessID {
		t.Fatalf("second run created a new session %q, want %q", sessID2, sessID)
	}

	// p.started is a ONE-SHOT signal (it closes on the first Stream call), so
	// startRunAsync returns immediately for run 2 — the attach below could beat
	// run 2's Submit and resolve against the settled run 1, whose terminal
	// attach path serves no session_state frame. Wait until run 2 is actually
	// the session's active (in-flight) run before attaching.
	waitFor(t, func() bool {
		run, active, err := rt.ActiveRun(context.Background(), sessID)
		return err == nil && active && !run.Status.Terminal()
	}, "run 2 never became the session's active run")

	attached := make(chan string, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		attached <- rec.Body.String()
	}()
	// Subscribers include the submitter, so two means the resumer has attached.
	waitFor(t, func() bool { return cb.subscribers(sessID) >= 2 }, "attach never subscribed to the broker")
	close(p.gate)

	select {
	case body := <-attached:
		if !strings.Contains(body, `"type":"data-session-state"`) {
			t.Errorf("attached SSE body missing session-scoped data-session-state frame\n---\n%s", body)
		}
		if !strings.Contains(body, `"key":"plan"`) {
			t.Errorf("attached SSE body missing plan key\n---\n%s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attached client did not receive the stream")
	}
}
