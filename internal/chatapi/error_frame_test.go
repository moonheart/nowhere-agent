package chatapi

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestEmitLifecycleEventRendersErrorFrame is the unit half of the G1 fix: a
// durable lifecycle event of kind "error" must render the error frame (and the
// failed run-status frame) on the emitter. Before emitLifecycleEvent had a
// KindError arm, a failed run's terminal event was dropped here, so attached
// clients saw the stream end with finishReason "stop" — indistinguishable from
// success.
func TestEmitLifecycleEventRendersErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "text-1", thinkID: "reasoning-1"}

	payload, err := json.Marshal("boom")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/chat/resume", nil)
	emitLifecycleEvent(req, emitter, session.Event{RunID: "r1", Kind: string(agent.KindError), Payload: payload})

	body := rec.Body.String()
	for _, want := range []string{`"type":"error"`, `"errorText":"boom"`, `"status":"failed"`} {
		if !strings.Contains(body, want) {
			t.Errorf("lifecycle error frame missing %s\n---\n%s", want, body)
		}
	}
	// The emitter must also latch the terminal reason for finish().
	emitter.finish()
	if got := finishReasonOf(t, rec.Body.String()); got != "error" {
		t.Errorf("finishReason after lifecycle error = %q, want error", got)
	}
}

// TestEmitLifecycleEventStillMapsRunningDoneCancelled guards the pre-existing
// arms against a regression from adding the KindError case.
func TestEmitLifecycleEventStillMapsRunningDoneCancelled(t *testing.T) {
	cases := map[agent.EventKind]string{
		agent.KindRunning:   `"status":"running"`,
		agent.KindDone:      `"status":"done"`,
		agent.KindCancelled: `"status":"cancelled"`,
	}
	for kind, want := range cases {
		rec := httptest.NewRecorder()
		emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "text-1", thinkID: "reasoning-1"}
		req := httptest.NewRequest("POST", "/api/chat/resume", nil)
		emitLifecycleEvent(req, emitter, session.Event{RunID: "r1", Kind: string(kind)})
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: missing %s\n---\n%s", kind, want, rec.Body.String())
		}
	}
}

// failingProvider returns a stream error immediately, driving a failed run.
type failingProvider struct{ err error }

func (failingProvider) Name() string { return "failing" }
func (p failingProvider) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	return nil, p.err
}

func failingLoop(err error) LoopFactory {
	return func(ctx context.Context, system string) *agent.Loop {
		return agent.New(failingProvider{err: err}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}
}

// TestSubmitterSeesErrorFrameOnFailedRun is the end-to-end half of the G1 fix:
// a client whose POST /api/chat run fails must receive the error frame and a
// terminal finishReason of "error", then [DONE]. Before the fix the run's
// terminal KindError was dropped on the lifecycle path, so the submitter's
// stream ended as finishReason "stop" — indistinguishable from success.
func TestSubmitterSeesErrorFrameOnFailedRun(t *testing.T) {
	rt := session.NewRuntime(session.NewMemStore())
	h := NewHandler(failingLoop(fmt.Errorf("boom")), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"go"}]}`))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"type":"error"`, `"errorText":"stream: boom"`, `"status":"failed"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("submitter stream missing %q\n---\n%s", want, body)
		}
	}
	if got := finishReasonOf(t, body); got != "error" {
		t.Errorf("finishReason = %q, want error", got)
	}
}

// TestMidRunAttacherSeesErrorFrame pins that a client attached to an in-flight
// run sees the terminal error frame live, not just a bare [DONE]. The provider
// is gated so the run is in-flight while the watcher attaches; releasing the
// gate makes the run fail, and the error must fan out to the attacher.
func TestMidRunAttacherSeesErrorFrame(t *testing.T) {
	cb := newCountingBroker(session.NewMemBroker(0))
	rt := session.NewRuntime(session.NewMemStore()).WithBroker(cb)
	p := newFailingGatedProvider("boom")
	h := NewHandler(gatedLoopFailing(p), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sessID, wait := startRunAsync(t, mux, p.gatedProvider, owner, `{"messages":[{"role":"user","content":"go"}]}`)
	defer wait()

	attached := make(chan string, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/chat/resume?threadId="+sessID+"&after=0", nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		attached <- rec.Body.String()
	}()
	// Subscribers include the submitter, so two means the attacher has attached.
	waitFor(t, func() bool { return cb.subscribers(sessID) >= 2 }, "attacher never subscribed to the broker")
	close(p.gate) // release → the run fails

	select {
	case body := <-attached:
		if !strings.Contains(body, `"type":"error"`) {
			t.Errorf("attacher missing error frame\n---\n%s", body)
		}
		if !strings.Contains(body, "data: [DONE]") {
			t.Errorf("attacher stream did not terminate\n---\n%s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attacher did not receive the terminal stream")
	}
}

// failingGatedProvider wraps gatedProvider so the run, once released, fails
// instead of completing. The first deltas stream normally; then the stream
// emits an error event.
type failingGatedProvider struct {
	*gatedProvider
}

func newFailingGatedProvider(errText string) *failingGatedProvider {
	return &failingGatedProvider{gatedProvider: newGatedProvider(errText)}
}

func gatedLoopFailing(p *failingGatedProvider) LoopFactory {
	return func(ctx context.Context, system string) *agent.Loop {
		return agent.New(p, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}
}

// Stream emits the start block, waits for the gate, then errors out.
func (p *failingGatedProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
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
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("boom")}
	}()
	return ch, nil
}
