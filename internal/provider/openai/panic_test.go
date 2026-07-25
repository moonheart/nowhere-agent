package openai

import (
	"testing"

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
	streamEvents(panicBody{}, out) // runs synchronously; must return, not crash

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
