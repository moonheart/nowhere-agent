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

// parkedProvider streams one text delta then waits on the run context, so the
// run stays in-flight until cancelled — exactly what the Stop button interrupts.
type parkedProvider struct {
	ctx   context.Context
	start chan struct{}
}

func (p *parkedProvider) Name() string { return "parked" }

func (p *parkedProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 4)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "thinking out loud"}
	close(p.start)
	go func() {
		defer close(ch)
		// Stay in-flight until the run context is cancelled, then stop producing.
		<-p.ctx.Done()
	}()
	return ch, nil
}

// startParkedChat begins a chat run whose provider blocks until cancelled, and
// returns once the run is mid-stream. Returns the session id and a func that
// waits for the handler goroutine to finish.
func startParkedChat(t *testing.T, mux *http.ServeMux, h *Handler, user identity.User) (sessID string, wait func()) {
	t.Helper()
	done := make(chan struct{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"run long"}]}`))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	go func() {
		defer close(done)
		mux.ServeHTTP(rec, req)
	}()
	// Give the handler a beat to resolve the session and start the run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sessionIDs(h)) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	ids := sessionIDs(h)
	if len(ids) == 0 {
		t.Fatal("chat run did not create a session")
	}
	sessID = ids[0]
	// Under decoupled run ownership the parked run no longer dies with the request
	// (its context is independent of any connection), so the test must cancel it to
	// unblock the streaming handler. wait cancels the run, then waits for the
	// handler goroutine to return.
	return sessID, func() {
		if h.registry != nil {
			h.registry.Cancel(sessID)
		}
		<-done
	}
}

// sessionIDs reads the sessions created so far for the fixed test user, via the
// handler's runtime (same package, so the unexported field is in scope).
func sessionIDs(h *Handler) []string {
	if h.runtime == nil {
		return nil
	}
	sessions, err := h.runtime.ListSessionsByUser(context.Background(), testUserID, "", 0, nil)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(sessions.Sessions))
	for _, s := range sessions.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

const testUserID = "cancel-user"

// newParkedHandler builds a handler whose loop blocks until the run context is
// cancelled. The provider captures the request context via the factory.
func newParkedHandler() (*Handler, *session.Runtime, *parkedProvider) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	pp := &parkedProvider{start: make(chan struct{})}
	factory := func(ctx context.Context, system string) *agent.Loop {
		pp.ctx = ctx
		return agent.New(pp, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}
	h := NewHandler(factory, "sys").WithRuntime(rt)
	return h, rt, pp
}

func TestCancelStopsInFlightRun(t *testing.T) {
	h, rt, pp := newParkedHandler()
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: testUserID}

	sessID, wait := startParkedChat(t, mux, h, user)
	<-pp.start // run is mid-stream

	// Cancel the active run via the endpoint (owner).
	req := httptest.NewRequest("POST", "/api/chat/cancel?threadId="+sessID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"cancelled":true`) {
		t.Errorf("cancel body = %s want cancelled:true", rec.Body.String())
	}

	wait() // the chat handler unwinds

	// The run settled as cancelled, not failed/done.
	runs, err := rt.RunsForSession(context.Background(), sessID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != session.RunCancelled {
		t.Errorf("run status = %+v want cancelled", runs)
	}
	// The lock is released: a new run can start.
	if _, err := rt.StartRun(context.Background(), sessID); err != nil {
		t.Errorf("expected new run after cancel, got %v", err)
	}
}

// TestCancelPersistsTerminalEvent is the regression test for the cancelled-run
// gap: the run's terminal KindCancelled must be persisted (and thus replayable /
// broadcastable to attached clients) even though the run's context is cancelled.
// The loop rides the cancelled runCtx, whose ctx.Err() would otherwise make both
// the SSE emit and the durable AppendEvent short-circuit — dropping the terminal
// event so attached clients never see the run end.
func TestCancelPersistsTerminalEvent(t *testing.T) {
	h, rt, pp := newParkedHandler()
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: testUserID}

	sessID, wait := startParkedChat(t, mux, h, user)
	<-pp.start

	req := httptest.NewRequest("POST", "/api/chat/cancel?threadId="+sessID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	mux.ServeHTTP(httptest.NewRecorder(), req)
	wait()

	runs, err := rt.RunsForSession(context.Background(), sessID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v n=%d", err, len(runs))
	}
	events, err := rt.Replay(context.Background(), runs[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawCancelled bool
	for _, e := range events {
		if e.Kind == string(agent.KindCancelled) {
			sawCancelled = true
		}
	}
	if !sawCancelled {
		kinds := make([]string, len(events))
		for i, e := range events {
			kinds[i] = e.Kind
		}
		t.Errorf("terminal cancelled event not persisted; run event kinds = %v", kinds)
	}
}

func TestCancelIdempotentWhenNoActiveRun(t *testing.T) {
	h, rt, _ := newParkedHandler()
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: testUserID}

	sess, err := rt.CreateSession(context.Background(), testUserID, "t")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/chat/cancel?threadId="+sess.ID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel(no run) status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"cancelled":false`) {
		t.Errorf("cancel(no run) body = %s want cancelled:false", rec.Body.String())
	}
}

func TestCancelForbiddenForForeignSession(t *testing.T) {
	h, _, pp := newParkedHandler()
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: testUserID}

	sessID, wait := startParkedChat(t, mux, h, owner)
	<-pp.start
	defer wait()

	intruder := identity.User{ID: "someone-else"}
	req := httptest.NewRequest("POST", "/api/chat/cancel?threadId="+sessID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), intruder))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign cancel status = %d want 403; body=%s", rec.Code, rec.Body.String())
	}

	// Clean up: owner cancels so the parked goroutine exits.
	cleanup := httptest.NewRequest("POST", "/api/chat/cancel?threadId="+sessID, nil)
	cleanup = cleanup.WithContext(identity.NewContextWithUser(cleanup.Context(), owner))
	mux.ServeHTTP(httptest.NewRecorder(), cleanup)
}

func TestCancelRequiresThreadID(t *testing.T) {
	h, _, _ := newParkedHandler()
	mux := http.NewServeMux()
	h.Register(mux)
	req := httptest.NewRequest("POST", "/api/chat/cancel", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("cancel without threadId status = %d want 400", rec.Code)
	}
}

// TestDeleteSessionCancelsActiveRun is the regression test for deleting a
// session with an in-flight run: the DELETE must cancel the run (like the
// Stop button) before soft-deleting the session, so a deleted conversation
// cannot keep generating headless in the background.
func TestDeleteSessionCancelsActiveRun(t *testing.T) {
	h, rt, pp := newParkedHandler()
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: testUserID}

	sessID, wait := startParkedChat(t, mux, h, user)
	<-pp.start // run is mid-stream

	req := httptest.NewRequest("DELETE", "/api/chat/sessions/"+sessID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}

	wait() // the chat handler unwinds once the run is cancelled

	// The run settled as cancelled, not failed/done — the delete interrupted it.
	runs, err := rt.RunsForSession(context.Background(), sessID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != session.RunCancelled {
		t.Errorf("run status = %+v want cancelled", runs)
	}
}

// dripProvider streams text deltas indefinitely until its ctx is cancelled —
// a generation that never finishes on its own, so the only way the run settles
// is via cancellation (Stop / client disconnect).
type dripProvider struct {
	start chan struct{}
}

func (p *dripProvider) Name() string { return "drip" }

func (p *dripProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event)
	go func() {
		defer close(ch)
		close(p.start)
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "word "}:
			}
		}
	}()
	return ch, nil
}

// TestDisconnectSettlesRun is the regression test for the run-leak: when the
// client goes away mid-run (request context cancelled), the loop must unwind
// and the run must reach a terminal state, not stay 'running' forever.
// TestDisconnectLeavesRunRunning verifies the decoupled-ownership behaviour
// (design D7): a client disconnecting detaches its stream and the handler
// returns, but the run keeps executing on its connection-independent worker —
// it is NOT cancelled the way it was when the run lived on the request context.
// Reaping abandoned runs is the session idle-end job (task 16.4), not disconnect.
func TestDisconnectLeavesRunRunning(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	pp := &dripProvider{start: make(chan struct{})}
	factory := func(ctx context.Context, system string) *agent.Loop {
		return agent.New(pp, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}
	h := NewHandler(factory, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)

	// Client request whose context we cancel to simulate the client going away.
	reqCtx, clientGone := context.WithCancel(context.Background())
	user := identity.User{ID: testUserID}
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"talk forever"}]}`))
	req = req.WithContext(identity.NewContextWithUser(reqCtx, user))

	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-pp.start // run is streaming

	// The client disconnects.
	clientGone()

	// The handler must unwind (its attach loop sees the dead request context).
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}

	// But the run must still be active: disconnect detaches, it does not cancel.
	page, err := rt.ListSessionsByUser(context.Background(), testUserID, "", 0, nil)
	if err != nil || len(page.Sessions) == 0 {
		t.Fatalf("list sessions: %v n=%d", err, len(page.Sessions))
	}
	if _, active, err := rt.ActiveRun(context.Background(), page.Sessions[0].ID); err != nil || !active {
		t.Errorf("run active = %v err=%v, want still active after disconnect", active, err)
	}
}

// TestHistoryActiveFlag verifies GET /history reports whether a run is still in
// flight, so the client only opts into resume (unstable_resume) for a genuinely
// unfinished run — resuming a completed one would duplicate the assistant reply.
func TestHistoryActiveFlag(t *testing.T) {
	h, rt, pp := newParkedHandler()
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: testUserID}

	sessID, wait := startParkedChat(t, mux, h, user)
	defer wait()
	<-pp.start // run mid-flight

	getActive := func() bool {
		req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sessID, nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("history status = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Active bool `json:"active"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Active
	}

	if !getActive() {
		t.Error("active = false while a run is in flight, want true")
	}

	// Finish the run (cancel it), then history must report inactive.
	if _, err := rt.CancelRun(context.Background(), sessID); err != nil {
		t.Fatal(err)
	}
	if getActive() {
		t.Error("active = true after the run settled, want false")
	}
}

// TestHistoryReportsActiveForSettledRun verifies GET /history reports a
// completed run as inactive, and that a follow-up resume does not re-stream the
// settled content (which is delivered by /history from the message store).
func TestHistoryReportsActiveForSettledRun(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newThinkingLoop, "sys").WithRuntime(rt).WithMessageStore(session.NewMemMessageStore())
	mux := http.NewServeMux()
	h.Register(mux)

	runChat(t, mux, `{"messages":[{"role":"user","content":"q1"}]}`)
	sessID := store.Sessions()[0].ID

	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sessID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d", rec.Code)
	}
	var resp struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Active {
		t.Error("active = true for a finished run, want false")
	}

	// Resume on the settled run streams nothing (content comes from /history).
	rreq := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
	rrec := httptest.NewRecorder()
	mux.ServeHTTP(rrec, rreq)
	if rrec.Code != http.StatusOK {
		t.Fatalf("resume status = %d", rrec.Code)
	}
	if strings.Contains(rrec.Body.String(), "text-delta") {
		t.Errorf("settled resume replayed content; want none (served by /history)\n%s", rrec.Body.String())
	}
}
