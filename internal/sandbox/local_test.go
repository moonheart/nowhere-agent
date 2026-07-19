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
