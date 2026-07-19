package provider

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// RawRecorder records raw LLM wire traffic: each API call's request body and
// (streamed) response body are written to disk verbatim. It captures the
// provider-native bytes exactly as they leave/arrive, before any canonical
// decoding — useful for debugging adapters and auditing what was actually sent.
//
// Recording is opt-in via a non-empty root dir. Auth headers are never
// recorded (bodies only). The zero value is a no-op recorder.
type RawRecorder struct {
	root string
	seq  atomic.Uint64
}

// NewRawRecorder returns a recorder writing under root, or a no-op recorder
// when root is empty. Each call gets its own <root>/<provider>/<ts>-<seq> pair.
func NewRawRecorder(root string) *RawRecorder {
	return &RawRecorder{root: root}
}

// Enabled reports whether recording is active.
func (r *RawRecorder) Enabled() bool { return r != nil && r.root != "" }

// Exchange captures one request/response pair. Call it with the marshalled
// request body; it returns a WriteCloser to tee the streaming response into.
// The returned closer finalizes the pair. On any filesystem error the exchange
// degrades to a pass-through (recording must never break a live request).
func (r *RawRecorder) Exchange(provider string, reqBody []byte) io.WriteCloser {
	if !r.Enabled() {
		return nopWriteCloser{}
	}
	seq := r.seq.Add(1)
	base := filepath.Join(r.root, provider, fmt.Sprintf("%s-%06d", time.Now().UTC().Format("20060102T150405.000000000"), seq))
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return nopWriteCloser{}
	}
	if err := os.WriteFile(base+".req", reqBody, 0o600); err != nil {
		return nopWriteCloser{}
	}
	f, err := os.Create(base + ".resp")
	if err != nil {
		return nopWriteCloser{}
	}
	return f
}

// nopWriteCloser discards writes; a no-op response sink when recording is off
// or the pair could not be opened.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
