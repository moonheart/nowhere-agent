package anthropic

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
)

// panicBody panics on Read, simulating a decode-time panic in the stream
// goroutine that would otherwise crash the process.
type panicBody struct{}

func (panicBody) Read([]byte) (int, error) { panic("read boom") }
func (panicBody) Close() error             { return nil }

// TestStreamEventsRecoversFromPanic verifies the stream goroutine turns a panic
// into an error event and closes cleanly rather than crashing the process.
func TestStreamEventsRecoversFromPanic(t *testing.T) {
	out := make(chan provider.Event, 4)
	streamEvents(context.Background(), panicBody{}, out) // runs synchronously; must return, not crash

	var sawErr bool
	for ev := range out {
		if ev.Type == provider.EventError {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected an EventError from a panicking stream body")
	}
}

// TestStreamEventsExitsOnCancel: the loop's consumer stops reading the moment
// a run is cancelled, so a producer blocked on a full channel must exit rather
// than leak forever (the deferred body.Close tears the HTTP stream down).
func TestStreamEventsExitsOnCancel(t *testing.T) {
	line := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"
	body := io.NopCloser(strings.NewReader(strings.Repeat(line, 100000)))

	out := make(chan provider.Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		streamEvents(ctx, body, out)
		close(done)
	}()

	// Consume one event, then cancel and STOP draining: the buffer fills and
	// the producer must still unwind.
	select {
	case <-out:
	case <-time.After(5 * time.Second):
		t.Fatal("no events produced")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamEvents blocked on a send after cancel")
	}
}
