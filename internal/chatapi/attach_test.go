package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// gatedProvider lets a test drive a run's event stream by hand: it emits a
// start block, then blocks sending deltas until the test releases them, so a
// run can be held in-flight while other clients attach. close(gate) finishes
// the stream.
type gatedProvider struct {
	started chan struct{} // closed when Stream is first called
	gate    chan struct{} // closed to release the run's content + end
	deltas  []string      // text deltas streamed once the gate opens
	mu      sync.Mutex
	calls   int
}

func newGatedProvider(deltas ...string) *gatedProvider {
	return &gatedProvider{
		started: make(chan struct{}),
		gate:    make(chan struct{}),
		deltas:  deltas,
	}
}

func (p *gatedProvider) Name() string { return "gated" }

func (p *gatedProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		close(p.started)
	}

	ch := make(chan provider.Event, 16)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
			return
		case <-p.gate:
		}
		for i, d := range p.deltas {
			select {
			case <-ctx.Done():
				return
			case ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: d}:
				_ = i
			}
		}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	}()
	return ch, nil
}

func gatedLoop(p *gatedProvider) LoopFactory {
	return func(ctx context.Context, system string) *agent.Loop {
		return agent.New(p, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}
}

// startRunAsync begins a chat run whose provider is gated (in-flight until the
// test opens the gate). It returns once the provider stream has started, with
// the session id and a func to wait for the handler goroutine.
func startRunAsync(t *testing.T, mux *http.ServeMux, p *gatedProvider, user identity.User, body string) (sessID string, wait func()) {
	t.Helper()
	done := make(chan struct{})
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	go func() {
		defer close(done)
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider stream did not start")
	}

	// Wait until the session exists in the runtime.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ids := sessionIDsForUser(t, mux, user.ID); len(ids) > 0 {
			return ids[0], func() { <-done }
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("chat run did not create a session")
	return "", nil
}

// sessionIDsForUser lists a user's sessions via the sessions endpoint.
func sessionIDsForUser(t *testing.T, mux *http.ServeMux, userID string) []string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/chat/sessions", nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), identity.User{ID: userID}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil
	}
	var resp struct {
		Sessions []sessionDTO `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return nil
	}
	ids := make([]string, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

// TestMultiClientAttachSameStream is the core 13.5 scenario: a second client
// attaching to a session with an in-flight run receives the run's event stream.
// The attach happens before the run's content is released, then we follow live;
// because the in-memory fan-out drops to a slow consumer, we assert on the
// lifecycle framing (running → done) and the merged content rather than every
// individual delta landing on the live channel.
func TestMultiClientAttachSameStream(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	p := newGatedProvider("alpha ", "beta ", "gamma")
	h := NewHandler(gatedLoop(p), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sessID, wait := startRunAsync(t, mux, p, owner, `{"messages":[{"role":"user","content":"go"}]}`)
	defer wait()

	// Second client attaches mid-run, replaying from the start.
	attached := make(chan string, 1)
	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		attached <- rec.Body.String()
	}()

	// Let the attach subscribe, then release the run's content to both clients.
	time.Sleep(50 * time.Millisecond)
	close(p.gate)

	select {
	case body := <-attached:
		// The attached client sees the run-start frame, at least the first
		// content delta, and a clean stream termination.
		if !strings.Contains(body, `"status":"running"`) {
			t.Errorf("attached client missing run-start frame\n---\n%s", body)
		}
		if !strings.Contains(body, `"delta":"alpha "`) {
			t.Errorf("attached client missing first delta\n---\n%s", body)
		}
		if !strings.Contains(body, "data: [DONE]") {
			t.Errorf("attached client stream did not terminate\n---\n%s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attached client did not receive the stream")
	}
	<-attachDone

	// A client attaching after the run completes gets a clean termination: the
	// settled content is delivered via /history (from the message store), not
	// re-streamed here (redis-stream-live).
	req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("post-completion attach should terminate cleanly\n---\n%s", body)
	}
	if strings.Contains(body, `"delta":"alpha "`) {
		t.Errorf("settled run must not re-stream content deltas (served by /history)\n---\n%s", body)
	}
}

// TestResumeActiveRunIgnoresAfterOffset is the regression test for the tab2
// duplicate-reply bug: resuming an IN-FLIGHT run must re-stream from the start
// even when the client passes a non-zero `after`. The follow renders ONE
// assistant message from the whole stream; honouring `after` for an active run
// would split it into a snapshot bubble plus a continuation bubble.
func TestResumeActiveRunIgnoresAfterOffset(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	p := newGatedProvider("alpha ", "beta")
	h := NewHandler(gatedLoop(p), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sessID, wait := startRunAsync(t, mux, p, owner, `{"messages":[{"role":"user","content":"go"}]}`)
	defer wait()

	// Attach mid-run (gate still shut) with a large `after`, then release the
	// run. The server must ignore `after` for the active run and stream from the
	// beginning (running + user + all content), not just the post-offset tail.
	attached := make(chan string, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=999", nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		attached <- rec.Body.String()
	}()
	time.Sleep(50 * time.Millisecond) // let the attach subscribe
	close(p.gate)

	var body string
	select {
	case body = <-attached:
	case <-time.After(3 * time.Second):
		t.Fatal("active-run resume did not return")
	}
	// The key assertion: the stream began from the run's start (running marker +
	// first delta), proving `after=999` was ignored for the active run. Later
	// deltas may drop to a slow test consumer, so don't require every one.
	for _, want := range []string{`"status":"running"`, `"delta":"alpha "`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("active-run resume(after=999) missing %q — must stream from start\n---\n%s", want, body)
		}
	}
}

// TestConcurrentRunStartConflict verifies single-active-run / multi-writer
// prevention: a second client submitting while a run is in flight is rejected
// with 409, and only ONE run exists.
func TestConcurrentRunStartConflict(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	p := newGatedProvider("x")
	h := NewHandler(gatedLoop(p), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sessID, wait := startRunAsync(t, mux, p, owner, `{"messages":[{"role":"user","content":"first"}]}`)
	defer wait()

	// A second client submits to the same session while the run is in flight.
	req := httptest.NewRequest("POST", "/api/chat",
		strings.NewReader(`{"threadId":"`+sessID+`","messages":[{"role":"user","content":"second"}]}`))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("concurrent run start = %d want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Only one run exists on the session (the in-flight one).
	if runs := store.RunsFor(sessID); len(runs) != 1 {
		t.Errorf("expected 1 run after rejected concurrent start, got %d", len(runs))
	}

	close(p.gate) // let the in-flight run finish
}

// TestCancelBroadcastToAttachedClients verifies cancelling a run terminates the
// stream of every attached client with a cancelled lifecycle frame, not just
// the submitter's.
func TestCancelBroadcastToAttachedClients(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	p := newGatedProvider("never released")
	h := NewHandler(gatedLoop(p), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sessID, wait := startRunAsync(t, mux, p, owner, `{"messages":[{"role":"user","content":"go"}]}`)
	defer wait()

	// Two extra clients attach.
	const watchers = 2
	bodies := make([]chan string, watchers)
	for i := range bodies {
		bodies[i] = make(chan string, 1)
		go func(out chan<- string) {
			req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
			req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			out <- rec.Body.String()
		}(bodies[i])
	}
	time.Sleep(50 * time.Millisecond) // let watchers subscribe

	// Cancel the run; the gate stays shut — cancellation, not completion, must
	// terminate every attached stream with a cancelled frame.
	creq := httptest.NewRequest("POST", "/api/chat/cancel?threadId="+sessID, nil)
	creq = creq.WithContext(identity.NewContextWithUser(creq.Context(), owner))
	mux.ServeHTTP(httptest.NewRecorder(), creq)

	for i, ch := range bodies {
		select {
		case body := <-ch:
			if !strings.Contains(body, `"status":"cancelled"`) {
				t.Errorf("watcher %d missing cancelled frame\n---\n%s", i, body)
			}
			if !strings.Contains(body, "data: [DONE]") {
				t.Errorf("watcher %d stream did not terminate\n---\n%s", i, body)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("watcher %d did not observe cancellation", i)
		}
	}
}

// TestRuntimeFanoutToManySubscribers verifies the Runtime's live broker fans
// each content frame out to every subscriber channel (the in-memory attach
// mechanism for streaming deltas).
func TestRuntimeFanoutToManySubscribers(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore())
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}
	run, err := rt.StartRun(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	const subs = 3
	chans := make([]<-chan session.StreamEvent, subs)
	unsubs := make([]func(), subs)
	for i := range chans {
		chans[i], unsubs[i] = rt.Broker().Subscribe(sess.ID, 8)
		defer unsubs[i]()
	}

	if err := rt.AppendEvent(context.Background(), session.Event{
		RunID: run.ID, SessionID: sess.ID, Kind: string(agent.KindText), Payload: []byte(`"hi"`),
	}); err != nil {
		t.Fatal(err)
	}

	for i, ch := range chans {
		select {
		case e := <-ch:
			if e.Kind != string(agent.KindText) {
				t.Errorf("subscriber %d got kind %q", i, e.Kind)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d did not receive the fanned-out frame", i)
		}
	}
}
