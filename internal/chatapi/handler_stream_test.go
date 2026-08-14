package chatapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

// deadlineRecorder is a ResponseWriter+Flusher that records SetWriteDeadline so
// a test can assert the streaming path cleared the write deadline.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline    time.Time
	deadlineSet bool
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadline = t
	d.deadlineSet = true
	return nil
}

// TestWriteStreamHeadersArmsRollingDeadline verifies a streaming response arms
// a rolling write deadline (not zeroed): the server WriteTimeout can't truncate
// a long SSE stream mid-run, while a stalled frame write still ends the stream.
func TestWriteStreamHeadersArmsRollingDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	if !writeStreamHeaders(rec) {
		t.Fatal("writeStreamHeaders returned false for a flushable writer")
	}
	if !rec.deadlineSet {
		t.Error("expected a write deadline to be armed for a streaming response")
	}
	left := time.Until(rec.deadline)
	if left <= 0 || left > streamWriteTimeout+time.Second {
		t.Errorf("write deadline = %v (in %v), want within %v in the future", rec.deadline, left, streamWriteTimeout)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestEmitterWriteRefreshesRollingDeadline verifies every frame write re-arms
// the rolling deadline, so a live stream never stalls out while a half-open
// client's single stalled write still ends the stream.
func TestEmitterWriteRefreshesRollingDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	e := newSSEEmitter(rec, rec, "m", "t", "r")
	// A frame write must have re-armed the deadline (a fresh one, in the future).
	firstWrite := time.Now()
	e.write(chunk{"type": "text", "id": "t", "delta": "x"})
	afterFirst := rec.deadline
	if !rec.deadlineSet || time.Until(afterFirst) <= 0 {
		t.Fatalf("first write did not arm a rolling deadline (deadline=%v)", rec.deadline)
	}
	// Writes through writeRaw (the write path's choke point) refresh it too.
	// Wait for the clock to advance past the first write's instant: the second
	// deadline is firstWrite+timeout+ε, which only STRICTLY exceeds the first
	// once time has moved on (a same-instant write would merely equal it).
	waitFor(t, func() bool { return time.Now().After(firstWrite) }, "clock did not advance past the first write")
	e.writeRaw("data: x\n\n")
	if !rec.deadline.After(afterFirst) {
		t.Errorf("deadline after second write = %v, want refreshed past %v", rec.deadline, afterFirst)
	}
}
