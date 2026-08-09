package provider

import (
	"fmt"
	"io"
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
type stallReader struct {
	body    io.ReadCloser
	timeout time.Duration
	err     error // sticky terminal error (the stall)
}

// NewStallReader wraps body with a per-read idle deadline. timeout<=0 returns
// body unchanged (stall detection disabled).
func NewStallReader(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 {
		return body
	}
	return &stallReader{body: body, timeout: timeout}
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	type result struct {
		n   int
		err error
	}
	// Buffered so the abandoned read goroutine never leaks after a stall.
	ch := make(chan result, 1)
	go func() {
		n, err := r.body.Read(p)
		ch <- result{n, err}
	}()
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-timer.C:
		r.err = &StreamStallError{Idle: r.timeout}
		_ = r.body.Close() // unblock the pending read; the goroutine exits
		return 0, r.err
	}
}

func (r *stallReader) Close() error { return r.body.Close() }
