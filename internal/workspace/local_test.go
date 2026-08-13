package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *LocalStore {
	t.Helper()
	s, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	return s
}

// writeTree creates files in dir per the map of relpath->content.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	files, err := listFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = string(b)
	}
	return out
}

func TestSolidifyThenMaterializeRoundTrip(t *testing.T) {
	s := newStore(t)
	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"a.txt":       "alpha",
		"sub/b.txt":   "beta",
		"sub/c/d.txt": "deep",
	})

	ref, err := s.Solidify("sess1", src)
	if err != nil {
		t.Fatalf("Solidify: %v", err)
	}
	if ref.Version != 1 {
		t.Errorf("first version = %d, want 1", ref.Version)
	}

	dst := t.TempDir()
	gotRef, err := s.Materialize("sess1", dst)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if gotRef.Version != 1 {
		t.Errorf("materialized version = %d", gotRef.Version)
	}
	got := readTree(t, dst)
	want := map[string]string{"a.txt": "alpha", filepath.Join("sub", "b.txt"): "beta", filepath.Join("sub", "c", "d.txt"): "deep"}
	if len(got) != len(want) {
		t.Fatalf("got %d files %v, want %d", len(got), got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("file %q = %q want %q", k, got[k], v)
		}
	}
}

func TestSolidifyIncrementsVersion(t *testing.T) {
	s := newStore(t)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f.txt": "v1"})
	r1, _ := s.Solidify("s", src)
	writeTree(t, src, map[string]string{"f.txt": "v2"})
	r2, _ := s.Solidify("s", src)

	if r1.Version != 1 || r2.Version != 2 {
		t.Errorf("versions = %d,%d want 1,2", r1.Version, r2.Version)
	}

	// Current should point at v2.
	cur, ok := s.Current("s")
	if !ok || cur.Version != 2 {
		t.Errorf("current = %+v ok=%v", cur, ok)
	}

	// Materialize gives latest content.
	dst := t.TempDir()
	s.Materialize("s", dst)
	got := readTree(t, dst)
	if got["f.txt"] != "v2" {
		t.Errorf("materialized content = %q want v2", got["f.txt"])
	}
}

func TestMaterializeWithNoVersion(t *testing.T) {
	s := newStore(t)
	dst := t.TempDir()
	ref, err := s.Materialize("empty", dst)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if ref.Version != 0 {
		t.Errorf("expected version 0 for fresh session, got %d", ref.Version)
	}
}

func TestSolidifyIsAtomicAgainstInterruption(t *testing.T) {
	s := newStore(t)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f.txt": "committed"})
	if _, err := s.Solidify("s", src); err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted solidify: leave junk in staging but never commit.
	staging := s.stagingDir("s")
	os.MkdirAll(staging, 0o755)
	writeTree(t, staging, map[string]string{"f.txt": "partial-corrupt"})

	// Current must still be the committed version.
	cur, ok := s.Current("s")
	if !ok || cur.Version != 1 {
		t.Fatalf("current = %+v ok=%v after interruption", cur, ok)
	}
	dst := t.TempDir()
	s.Materialize("s", dst)
	got := readTree(t, dst)
	if got["f.txt"] != "committed" {
		t.Errorf("after interruption materialized %q want committed", got["f.txt"])
	}
}

func TestSolidifyRollsBackVersionDirOnPointerFailure(t *testing.T) {
	s := newStore(t)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f.txt": "v1"})
	if _, err := s.Solidify("s", src); err != nil {
		t.Fatal(err)
	}

	// Sabotage the pointer commit: a directory squatting on the temp path
	// makes os.WriteFile fail after the version dir was already promoted.
	cur := s.currentPath("s")
	if err := os.MkdirAll(cur+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Solidify("s", src); err == nil {
		t.Fatal("solidify must fail when the pointer cannot be committed")
	}

	// The promoted version dir must be rolled back so the next solidify
	// recomputes the same version instead of colliding with an existing dir.
	if _, err := os.Stat(filepath.Join(s.versionsDir("s"), "2")); !os.IsNotExist(err) {
		t.Fatalf("version 2 dir must be rolled back on pointer failure (stat err = %v)", err)
	}
	// Current still points at the committed version.
	if cur, ok := s.Current("s"); !ok || cur.Version != 1 {
		t.Fatalf("current = %+v ok=%v, want version 1", cur, ok)
	}

	if err := os.RemoveAll(cur + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Solidify("s", src); err != nil {
		t.Fatalf("solidify after rollback: %v", err)
	}
	if ref, _ := s.Current("s"); ref.Version != 2 {
		t.Errorf("current version = %d, want 2", ref.Version)
	}
}

func TestSessionsIsolated(t *testing.T) {
	s := newStore(t)
	srcA := t.TempDir()
	srcB := t.TempDir()
	writeTree(t, srcA, map[string]string{"x": "A"})
	writeTree(t, srcB, map[string]string{"x": "B"})
	s.Solidify("sessA", srcA)
	s.Solidify("sessB", srcB)

	dst := t.TempDir()
	s.Materialize("sessA", dst)
	if got := readTree(t, dst); got["x"] != "A" {
		t.Errorf("sessA materialized %q want A", got["x"])
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f": "x"})
	s.Solidify("s", src)
	if err := s.Delete("s"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Current("s"); ok {
		t.Error("expected no current after delete")
	}
}

func TestMeta(t *testing.T) {
	s := newStore(t)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a": "1234", "b": "56"})
	ref, _ := s.Solidify("s", src)
	m, err := s.Meta(ref)
	if err != nil {
		t.Fatal(err)
	}
	if m.Files != 2 || m.Bytes != 6 {
		t.Errorf("meta = files %d bytes %d, want 2/6", m.Files, m.Bytes)
	}
}
