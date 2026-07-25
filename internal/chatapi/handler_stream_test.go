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

// TestWriteStreamHeadersClearsWriteDeadline verifies a streaming response clears
// its per-connection write deadline, so the server WriteTimeout can't truncate a
// long SSE stream mid-run.
func TestWriteStreamHeadersClearsWriteDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	if !writeStreamHeaders(rec) {
		t.Fatal("writeStreamHeaders returned false for a flushable writer")
	}
	if !rec.deadlineSet {
		t.Error("expected the write deadline to be cleared for a streaming response")
	}
	if !rec.deadline.IsZero() {
		t.Errorf("write deadline = %v, want zero (no deadline)", rec.deadline)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}
