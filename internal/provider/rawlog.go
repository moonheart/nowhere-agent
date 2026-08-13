package provider

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxRawLogFileBytes caps one raw request/response file: an oversized LLM
// payload (huge tool results, materialized images) must not balloon the log
// dir, and the caller's live stream must never be slowed by the recording.
// Files past the cap are truncated with a marker so they stay self-describing.
const maxRawLogFileBytes = 8 << 20 // 8 MiB

// rawLogTruncationMarker is appended to a capped raw log file.
const rawLogTruncationMarker = "\n... [TRUNCATED: raw log file capped at 8 MiB]\n"

// RawRecorder records raw LLM wire traffic: each API call's request body and
// (streamed) response body are written to disk verbatim. It captures the
// provider-native bytes exactly as they leave/arrive, before any canonical
// decoding — useful for debugging adapters and auditing what was actually sent.
//
// Recording is opt-in via a non-empty root dir. Auth headers are never
// recorded (bodies only). The zero value is a no-op recorder. The root is
// mutable via SetRoot so the admin console can turn recording on/off and
// retarget it without a restart; Exchange snapshots the current root.
type RawRecorder struct {
	mu   sync.RWMutex
	root string
	seq  atomic.Uint64
}

// NewRawRecorder returns a recorder writing under root, or a no-op recorder
// when root is empty. Each call gets its own <root>/<provider>/<ts>-<seq> pair.
func NewRawRecorder(root string) *RawRecorder {
	return &RawRecorder{root: root}
}

// SetRoot retunes the recording root live. An empty root disables recording;
// a non-empty root enables it for subsequent exchanges.
func (r *RawRecorder) SetRoot(root string) {
	r.mu.Lock()
	r.root = root
	r.mu.Unlock()
}

// Enabled reports whether recording is active.
func (r *RawRecorder) Enabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r != nil && r.root != ""
}

// Exchange captures one request/response pair. Call it with the marshalled
// request body; it returns a WriteCloser to tee the streaming response into.
// The returned closer finalizes the pair. On any filesystem error the exchange
// degrades to a pass-through (recording must never break a live request).
func (r *RawRecorder) Exchange(provider string, reqBody []byte) io.WriteCloser {
	r.mu.RLock()
	root := r.root
	r.mu.RUnlock()
	if root == "" {
		return nopWriteCloser{}
	}
	seq := r.seq.Add(1)
	base := filepath.Join(root, provider, fmt.Sprintf("%s-%06d", time.Now().UTC().Format("20060102T150405.000000000"), seq))
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return nopWriteCloser{}
	}
	if err := writeRawLogFile(base+".req", reqBody); err != nil {
		return nopWriteCloser{}
	}
	f, err := os.Create(base + ".resp")
	if err != nil {
		return nopWriteCloser{}
	}
	return &truncatingFile{f: f}
}

// writeRawLogFile writes data to path, capped at maxRawLogFileBytes with a
// truncation marker appended past the cap.
func writeRawLogFile(path string, data []byte) error {
	if len(data) <= maxRawLogFileBytes {
		return os.WriteFile(path, data, 0o600)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data[:maxRawLogFileBytes]); err != nil {
		return err
	}
	_, err = f.WriteString(rawLogTruncationMarker)
	return err
}

// truncatingFile is an io.WriteCloser over a raw log file that swallows writes
// past maxRawLogFileBytes — a tee'd stream must never fail because the log is
// full (recording never breaks a live request) — and appends a truncation
// marker on Close when the cap was hit.
type truncatingFile struct {
	f         *os.File
	written   int64
	truncated bool
}

func (t *truncatingFile) Write(p []byte) (int, error) {
	if t.written >= maxRawLogFileBytes {
		t.truncated = true
		return len(p), nil // swallow: the sink must never error the live stream
	}
	room := maxRawLogFileBytes - t.written
	if int64(len(p)) > room {
		t.truncated = true
		if n, err := t.f.Write(p[:room]); err != nil {
			return n, err
		}
		t.written = maxRawLogFileBytes
		return len(p), nil
	}
	n, err := t.f.Write(p)
	t.written += int64(n)
	return n, err
}

func (t *truncatingFile) Close() error {
	var err error
	if t.truncated {
		_, err = t.f.WriteString(rawLogTruncationMarker)
	}
	if cerr := t.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Sweep deletes raw log files under the recording root whose mtime predates
// before. Only *.req / *.resp files are touched — the root may hold other
// operator files — and a broken entry never aborts the rest of the walk.
// Returns the number of files removed. A no-op when recording is off.
func (r *RawRecorder) Sweep(before time.Time) (int, error) {
	r.mu.RLock()
	root := r.root
	r.mu.RUnlock()
	if root == "" {
		return 0, nil
	}
	removed := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // keep walking past a broken entry
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if !strings.HasSuffix(path, ".req") && !strings.HasSuffix(path, ".resp") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(before) {
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
		return nil
	})
	return removed, err
}

// nopWriteCloser discards writes; a no-op response sink when recording is off
// or the pair could not be opened.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
