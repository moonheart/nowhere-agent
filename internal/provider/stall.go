package provider

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// StreamStallError marks a provider stream that went silent: no bytes arrived
// for the configured idle window, so the connection is presumed dead (a hung
// upstream that never sends FIN would otherwise block the run until the outer
// context is cancelled). The adapters emit it as an EventError; it is distinct
// from ctx cancellation so the loop can log it as an upstream fault.
type StreamStallError struct {
	Idle time.Duration
}

func (e *StreamStallError) Error() string {
	return fmt.Sprintf("provider stream stalled: no data for %s", e.Idle)
}

// stallReader wraps a streaming response body with a wall-clock deadline on
// EACH read: if no bytes arrive within timeout, it closes the body (unblocking
// the abandoned read) and reports a *StreamStallError. This detects a silent
// upstream stall, which neither the HTTP client's timeouts nor SSE decoding
// can see.
//
// A single watchdog goroutine serves the reader's whole life: a Read arms a
// deadline (read start + timeout) and the watchdog fires the stall when that
// deadline passes with the read still pending. The window is DISARMED when a
// read completes, so a pause between reads (consumer backpressure) never
// trips the stall — the deadline only counts while a read is actually waiting
// on the body, exactly like a per-read timer. The old design spent a
// goroutine + channel + timer on every Read (one per token on a streaming
// response); this is one goroutine and one reusable timer for the stream, so
// the hot read path is allocation-free.
type stallReader struct {
	body    io.ReadCloser
	timeout time.Duration

	mu       sync.Mutex
	deadline time.Time // end of the armed window; zero while no read is pending
	fired    bool      // the stall fired; sticky for every later read
	err      *StreamStallError
	closed   bool // Close() called, or the stream ended with EOF: watchdog exits

	wake chan struct{} // buffered(1): tells the watchdog to re-check
}

// NewStallReader wraps body with a per-read idle deadline. timeout<=0 returns
// body unchanged (stall detection disabled).
func NewStallReader(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return body
	}
	r := &stallReader{body: body, timeout: timeout, wake: make(chan struct{}, 1)}
	go r.watchdog()
	return r
}

func (r *stallReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.fired {
		err := r.err
		r.mu.Unlock()
		return 0, err
	}
	// Arm the window for this read. The stall fires only while a read is
	// pending, so the deadline is disarmed again when the read completes.
	r.deadline = time.Now().Add(r.timeout)
	r.signalLocked()
	r.mu.Unlock()

	n, err := r.body.Read(p)

	r.mu.Lock()
	if r.fired {
		n, err = 0, r.err // the watchdog closed the body; report the stall
	}
	if err == io.EOF {
		r.closed = true // stream over; let the watchdog exit
	}
	r.deadline = time.Time{}
	r.signalLocked()
	r.mu.Unlock()
	return n, err
}

// signalLocked nudges the watchdog to re-check the deadline. Buffered, so the
// send never blocks; a pending wake merely means "re-check" and duplicates
// are harmless. Caller holds mu.
func (r *stallReader) signalLocked() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// watchdog waits for an armed deadline and fires the stall when it passes
// with a read still pending, then closes the body to unblock it. It exits on
// Close/EOF and after firing — one goroutine for the stream's whole life.
func (r *stallReader) watchdog() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	_ = timer.Stop()
	for {
		r.mu.Lock()
		dl := r.deadline
		done := r.fired || r.closed
		r.mu.Unlock()
		if done {
			return
		}
		if dl.IsZero() {
			<-r.wake // park until a read arms a deadline
			continue
		}
		wait := time.Until(dl)
		if wait > 0 {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(wait)
			select {
			case <-r.wake: // re-armed (or disarmed); re-evaluate
			case <-timer.C: // deadline hit; re-verify under the lock
			}
			continue
		}
		// The deadline passed. Re-verify under the lock that a read is still
		// pending and the deadline is still the armed one: a stale timer fire
		// after the read completed must not trip the stall.
		r.mu.Lock()
		fire := !r.fired && !r.closed && !r.deadline.IsZero() && time.Until(r.deadline) <= 0
		if fire {
			r.fired = true
			r.err = &StreamStallError{Idle: r.timeout}
		}
		r.mu.Unlock()
		if fire {
			_ = r.body.Close() // unblock the pending read; it reports the stall
			return
		}
	}
}

func (r *stallReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.signalLocked()
	r.mu.Unlock()
	return r.body.Close()
}
