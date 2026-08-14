package chatapi

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEmitterPingWritesCommentFrame verifies the heartbeat frame is an SSE
// comment (":"-prefixed line, not "data:"), so EventSource and the
// assistant-ui decoder ignore it while the connection stays alive.
func TestEmitterPingWritesCommentFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}
	emitter.ping()
	body := rec.Body.String()
	if want := ": ping\n\n"; body != want {
		t.Fatalf("ping body = %q, want %q", body, want)
	}
	if strings.Contains(body, "data:") {
		t.Errorf("ping frame must not be a data frame: %q", body)
	}
}

// TestEmitterPingLatchesWriteErr verifies a ping after a failed write is a
// no-op (the writeErr latch is shared with the data path).
func TestEmitterPingLatchesWriteErr(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := &sseEmitter{w: rec, flusher: rec, msgID: "m", textID: "t", thinkID: "r"}
	emitter.writeErr = errors.New("closed")
	emitter.ping()
	if rec.Body.Len() != 0 {
		t.Fatalf("ping wrote after a latched write error: %q", rec.Body.String())
	}
}
