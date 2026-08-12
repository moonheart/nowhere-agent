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

// TestFillGapRecoversDroppedFrames verifies the attach-side gap compensation:
// when a live frame skips offsets (a slow consumer's frames were dropped), the
// retained gap frames are re-read and emitted in offset order, bounded below
// the live frame the caller emits next.
func TestFillGapRecoversDroppedFrames(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(nil, "sys").WithRuntime(rt)
	broker := rt.Broker()

	// Retain offsets 1..6 for the session.
	ctx := context.Background()
	for i := 1; i <= 6; i++ {
		if _, err := broker.Publish(ctx, "s1", session.StreamEvent{RunID: "r1", Kind: "text", Payload: []byte{byte('0' + i)}}); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}
	req := httptest.NewRequest("GET", "/", nil)

	// The consumer saw offsets 1 and 2, then frame 6 arrived live (3..5 were
	// dropped): fillGap must emit 3,4,5 and return maxOffset=5.
	got := h.fillGap(req, emitter, broker, "s1", "r1", 2, 6)
	if got != 5 {
		t.Fatalf("fillGap maxOffset = %d, want 5", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"3", "4", "5"} {
		if !strings.Contains(body, `"textDelta":"`+want+`"`) {
			t.Errorf("gap frame %q not emitted\n%s", want, body)
		}
	}
	// Frame 6 is the caller's live frame — fillGap must NOT emit it.
	if strings.Contains(body, `"textDelta":"6"`) {
		t.Errorf("fillGap emitted the caller's live frame (offset 6)\n%s", body)
	}
}

// TestFillGapConsecutiveOffsetsNoop verifies a stream with no holes leaves
// maxOffset unchanged and emits nothing.
func TestFillGapConsecutiveOffsetsNoop(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(nil, "sys").WithRuntime(rt)
	broker := rt.Broker()
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := broker.Publish(ctx, "s1", session.StreamEvent{RunID: "r1", Kind: "text", Payload: []byte{byte('0' + i)}}); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}
	req := httptest.NewRequest("GET", "/", nil)

	// Offsets 1..3 seen; next live frame is 4 — consecutive, nothing to fill.
	got := h.fillGap(req, emitter, broker, "s1", "r1", 3, 4)
	if got != 3 {
		t.Fatalf("fillGap maxOffset = %d, want 3 (unchanged)", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("no-gap fill emitted frames: %q", rec.Body.String())
	}
}

// TestFillGapSkipsOtherRuns verifies gap frames from a different run (or the
// same run already consumed) are skipped, keeping maxOffset pinned to this
// run's stream.
func TestFillGapSkipsOtherRuns(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(nil, "sys").WithRuntime(rt)
	broker := rt.Broker()
	ctx := context.Background()
	// Offsets 1..2 belong to run r1, 3..4 to run r2.
	_, _ = broker.Publish(ctx, "s1", session.StreamEvent{RunID: "r1", Kind: "text", Payload: []byte("a")})
	_, _ = broker.Publish(ctx, "s1", session.StreamEvent{RunID: "r1", Kind: "text", Payload: []byte("b")})
	_, _ = broker.Publish(ctx, "s1", session.StreamEvent{RunID: "r2", Kind: "text", Payload: []byte("c")})
	_, _ = broker.Publish(ctx, "s1", session.StreamEvent{RunID: "r2", Kind: "text", Payload: []byte("d")})

	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}
	req := httptest.NewRequest("GET", "/", nil)

	// Consumer follows run r1, saw offset 1, next live frame is 5 (r2's frames
	// dropped along with r1's 2): only r1's offset 2 may be filled.
	got := h.fillGap(req, emitter, broker, "s1", "r1", 1, 5)
	if got != 2 {
		t.Fatalf("fillGap maxOffset = %d, want 2", got)
	}
	if !strings.Contains(rec.Body.String(), `"textDelta":"b"`) {
		t.Errorf("r1 gap frame missing\n%s", rec.Body.String())
	}
	for _, skip := range []string{"c", "d"} {
		if strings.Contains(rec.Body.String(), `"textDelta":"`+skip+`"`) {
			t.Errorf("other-run frame %q emitted\n%s", skip, rec.Body.String())
		}
	}
}

// TestSessionActiveEndpoint verifies GET /api/chat/sessions/{id}/active returns
// {active:true} while a run is in flight and {active:false} after it settles.
func TestSessionActiveEndpoint(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(nil, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: "u1"}

	sess, err := rt.CreateSession(context.Background(), user.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	getActive := func() bool {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/chat/sessions/"+sess.ID+"/active", nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("active status = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Active bool `json:"active"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Active
	}

	if getActive() {
		t.Fatal("active before any run = true, want false")
	}
	if _, err := rt.StartRun(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if !getActive() {
		t.Fatal("active during a run = false, want true")
	}
	if err := rt.CompleteRun(context.Background(), sess.ID, session.RunDone); err != nil {
		t.Fatal(err)
	}
	if getActive() {
		t.Fatal("active after settle = true, want false")
	}
}
