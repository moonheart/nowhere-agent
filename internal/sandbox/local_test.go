package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLocalSandbox(t *testing.T) (*LocalPort, Handle) {
	t.Helper()
	root := t.TempDir()
	p := NewLocalPort(root)
	h, err := p.Create(context.Background(), "sess1", Options{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return p, h
}

func TestLocalPortWriteReadRoundTrip(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()

	if err := p.WriteFile(ctx, h, "notes/hello.txt", strings.NewReader("hi there")); err != nil {
		t.Fatalf("write: %v", err)
	}
	rc, err := p.ReadFile(ctx, h, "notes/hello.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "hi there" {
		t.Errorf("content = %q", b)
	}
}

func TestLocalPortListDir(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	_ = p.WriteFile(ctx, h, "a.txt", strings.NewReader("a"))
	_ = p.WriteFile(ctx, h, "sub/b.txt", strings.NewReader("b"))

	names, err := p.ListDir(ctx, h, ".")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "a.txt") || !strings.Contains(joined, "sub") {
		t.Errorf("list = %v", names)
	}
}

func TestLocalPortMove(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	_ = p.WriteFile(ctx, h, "old.txt", strings.NewReader("data"))

	if err := p.Move(ctx, h, "old.txt", "sub/new.txt"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := p.ReadFile(ctx, h, "old.txt"); err == nil {
		t.Error("source should be gone after move")
	}
	rc, err := p.ReadFile(ctx, h, "sub/new.txt")
	if err != nil {
		t.Fatalf("read moved file: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "data" {
		t.Errorf("moved content = %q", b)
	}
}

func TestLocalPortCopyRecursive(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	_ = p.WriteFile(ctx, h, "dir/a.txt", strings.NewReader("a"))
	_ = p.WriteFile(ctx, h, "dir/nested/b.txt", strings.NewReader("b"))

	if err := p.Copy(ctx, h, "dir", "dir2"); err != nil {
		t.Fatalf("copy dir: %v", err)
	}
	// Source intact and destination has the recursive copy.
	for path, want := range map[string]string{"dir/a.txt": "a", "dir/nested/b.txt": "b", "dir2/a.txt": "a", "dir2/nested/b.txt": "b"} {
		rc, err := p.ReadFile(ctx, h, path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != want {
			t.Errorf("%s = %q, want %q", path, b, want)
		}
	}
}

func TestLocalPortDeleteAndMkdir(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	_ = p.WriteFile(ctx, h, "tree/a.txt", strings.NewReader("a"))

	if err := p.Delete(ctx, h, "tree"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := p.ReadFile(ctx, h, "tree/a.txt"); err == nil {
		t.Error("tree/a.txt should be deleted")
	}

	if err := p.Mkdir(ctx, h, "x/y/z"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	info, err := os.Stat(filepath.Join(p.root, h.SessionID, "x", "y", "z"))
	if err != nil || !info.IsDir() {
		t.Errorf("mkdir did not create nested dir: %v", err)
	}
}

func TestLocalPortMutationRejectsEscape(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	if err := p.Move(ctx, h, "a.txt", "../out.txt"); err == nil {
		t.Error("expected error moving to ../out.txt")
	}
	if err := p.Copy(ctx, h, "a.txt", "/tmp/evil"); err == nil {
		t.Error("expected error copying to absolute path")
	}
	if err := p.Delete(ctx, h, "../../something"); err == nil {
		t.Error("expected error deleting ../../something")
	}
	if err := p.Mkdir(ctx, h, "../escape"); err == nil {
		t.Error("expected error mkdir ../escape")
	}
}

func TestLocalPortRejectsDotDotEscape(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()

	if err := p.WriteFile(ctx, h, "../evil.txt", strings.NewReader("x")); err == nil {
		t.Error("expected error writing ../evil.txt")
	}
	if _, err := p.ReadFile(ctx, h, "../../etc/passwd"); err == nil {
		t.Error("expected error reading ../../etc/passwd")
	}
	if _, err := p.ListDir(ctx, h, ".."); err == nil {
		t.Error("expected error listing ..")
	}
	// Confirm nothing was written outside the workspace root.
	if _, err := os.Stat(filepath.Join(p.root, "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("escape file exists outside workspace: %v", err)
	}
}

func TestLocalPortRejectsAbsolutePath(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	abs := filepath.Join(t.TempDir(), "abs.txt")
	if err := p.WriteFile(ctx, h, abs, strings.NewReader("x")); err == nil {
		t.Error("expected error on absolute path write")
	}
}

func TestLocalPortRejectsSymlinkEscape(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()

	// Plant a file outside the workspace, then a symlink inside the workspace
	// pointing at it. resolve must refuse to follow it.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(p.root, h.SessionID)
	link := filepath.Join(ws, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink (Windows perms): %v", err)
	}
	if _, err := p.ReadFile(ctx, h, "link.txt"); err == nil {
		t.Error("expected error reading symlink that escapes workspace")
	}
}

func TestLocalPortDestroy(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	_ = p.WriteFile(ctx, h, "x.txt", strings.NewReader("x"))
	if err := p.Destroy(ctx, h); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.root, h.SessionID)); !os.IsNotExist(err) {
		t.Errorf("workspace still exists after destroy: %v", err)
	}
}

func TestLocalPortManagerLifecycle(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(NewLocalPort(root))
	ctx := context.Background()

	h, err := mgr.Ensure(ctx, "s1", Options{})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Ensure is idempotent for a running session.
	h2, err := mgr.Ensure(ctx, "s1", Options{})
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if h.ID != h2.ID {
		t.Errorf("expected same handle, got %q vs %q", h.ID, h2.ID)
	}
	mgr.MarkSessionEnded("s1", 0)
	destroyed, err := mgr.Sweep(ctx, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(destroyed) != 1 || destroyed[0] != "s1" {
		t.Errorf("destroyed = %v", destroyed)
	}
}
