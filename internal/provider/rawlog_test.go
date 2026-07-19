package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
