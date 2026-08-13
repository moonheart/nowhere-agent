package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRawRecorderDisabledByDefault(t *testing.T) {
	r := NewRawRecorder("")
	if r.Enabled() {
		t.Fatal("empty root must disable recording")
	}
	sink := r.Exchange("anthropic", []byte("{}"))
	if _, err := sink.Write([]byte("ignored")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	// no files written anywhere observable — nothing to assert beyond no panic.
}

func TestRawRecorderWritesPair(t *testing.T) {
	dir := t.TempDir()
	r := NewRawRecorder(dir)
	if !r.Enabled() {
		t.Fatal("non-empty root must enable recording")
	}

	sink := r.Exchange("anthropic", []byte(`{"model":"x"}`))
	if _, err := sink.Write([]byte("data: a\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("data: b\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "anthropic", "*.req"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected 1 .req file, got %v (%v)", files, err)
	}
	req, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(req) != `{"model":"x"}` {
		t.Errorf("req body = %q", req)
	}

	resp, err := os.ReadFile(strings.TrimSuffix(files[0], ".req") + ".resp")
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "data: a\n\ndata: b\n\n" {
		t.Errorf("resp body = %q", resp)
	}
}

// TestRawRecorderPairFileModes pins that BOTH sides of a recorded pair are
// created 0600: the .resp holds the full model output and must not be
// world/group readable (the .req side was already 0600). Mode bits are not
// enforced on Windows, so the assertion runs on POSIX only.
func TestRawRecorderPairFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not enforced on Windows")
	}
	dir := t.TempDir()
	r := NewRawRecorder(dir)

	sink := r.Exchange("anthropic", []byte(`{"model":"x"}`))
	if _, err := sink.Write([]byte("data: a\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "anthropic", "*.req"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected 1 .req file, got %v (%v)", files, err)
	}
	for _, p := range []string{files[0], strings.TrimSuffix(files[0], ".req") + ".resp"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", p, perm)
		}
	}
}

func TestRawRecorderSequencesExchanges(t *testing.T) {
	dir := t.TempDir()
	r := NewRawRecorder(dir)
	for i := 0; i < 3; i++ {
		s := r.Exchange("openai", []byte("req"))
		_ = s.Close()
	}
	files, _ := filepath.Glob(filepath.Join(dir, "openai", "*.req"))
	if len(files) != 3 {
		t.Fatalf("expected 3 distinct exchanges, got %d", len(files))
	}
}

func TestRawRecorderSurvivesUnwritableRoot(t *testing.T) {
	// A file as root (not a dir) makes MkdirAll fail; recording must degrade to
	// a pass-through rather than break the live request.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRawRecorder(filepath.Join(f, "sub"))
	sink := r.Exchange("anthropic", []byte("{}"))
	if _, err := sink.Write([]byte("data")); err != nil {
		t.Fatalf("degraded sink must accept writes: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRawRecorderTruncatesHugeBodies: oversized request/response bodies are
// capped at maxRawLogFileBytes with a marker, and the sink keeps accepting
// (swallowing) writes past the cap so the tee'd live stream never errors.
func TestRawRecorderTruncatesHugeBodies(t *testing.T) {
	dir := t.TempDir()
	r := NewRawRecorder(dir)

	huge := make([]byte, maxRawLogFileBytes+4096)
	for i := range huge {
		huge[i] = 'x'
	}
	sink := r.Exchange("openai", huge)

	// The response stream continues far past the cap; the sink must keep
	// returning success (io.TeeReader propagates sink errors).
	chunk := make([]byte, 8192)
	for i := 0; i < 1500; i++ { // 12 MiB total, past the 8 MiB cap
		if _, err := sink.Write(chunk); err != nil {
			t.Fatalf("sink must swallow writes past the cap: %v", err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "openai", "*.req"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected 1 .req file, got %v (%v)", files, err)
	}
	req, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(req) >= len(huge) {
		t.Errorf("req file not capped: %d bytes", len(req))
	}
	if !strings.Contains(string(req), "TRUNCATED") {
		t.Errorf("req file lacks truncation marker")
	}
	resp, err := os.ReadFile(strings.TrimSuffix(files[0], ".req") + ".resp")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp) > maxRawLogFileBytes+len(rawLogTruncationMarker) {
		t.Errorf("resp file not capped: %d bytes", len(resp))
	}
	if !strings.HasSuffix(string(resp), rawLogTruncationMarker) {
		t.Errorf("resp file lacks trailing truncation marker")
	}
}

// TestRawRecorderSweepRemovesOnlyOldLogFiles: Sweep deletes stale *.req/*.resp
// files, keeps fresh ones, and never touches non-log files in the root.
func TestRawRecorderSweepRemovesOnlyOldLogFiles(t *testing.T) {
	dir := t.TempDir()
	r := NewRawRecorder(dir)

	// Old pair (mtime backdated beyond the cutoff).
	oldReq := filepath.Join(dir, "anthropic", "old-000001.req")
	oldResp := filepath.Join(dir, "anthropic", "old-000001.resp")
	// Fresh pair, and a non-log file that must survive regardless of age.
	freshReq := filepath.Join(dir, "anthropic", "fresh-000002.req")
	decoy := filepath.Join(dir, "keep.txt")
	for _, p := range []string{oldReq, oldResp, freshReq, decoy} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldReq, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldResp, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := r.Sweep(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	for _, p := range []string{oldReq, oldResp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale file %s must be gone, got %v", p, err)
		}
	}
	if _, err := os.Stat(freshReq); err != nil {
		t.Errorf("fresh file must survive: %v", err)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("non-log file must survive: %v", err)
	}
}
