package sandbox

import (
	"context"
	"io"
	"log/slog"
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

func TestLocalPortRejectsSymlinkedParentEscape(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()

	// A symlinked parent directory inside the workspace pointing outside it:
	// writes into not-yet-existing children must be refused.
	outside := t.TempDir()
	ws := filepath.Join(p.root, h.SessionID)
	link := filepath.Join(ws, "sub")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink (Windows perms): %v", err)
	}
	if err := p.WriteFile(ctx, h, "sub/evil.txt", strings.NewReader("x")); err == nil {
		t.Error("expected error writing through symlinked parent")
	}
	if err := p.Mkdir(ctx, h, "sub/deep"); err == nil {
		t.Error("expected error mkdir through symlinked parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("escape file exists outside workspace via symlinked parent: %v", err)
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

func TestBoundedCaptureTruncates(t *testing.T) {
	var b boundedCapture
	big := strings.Repeat("x", maxExecCaptureBytes)
	if _, err := b.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	// Exactly at the bound: no truncation yet.
	if got := b.String(); got != big {
		t.Errorf("within-bound output changed: len %d want %d", len(got), len(big))
	}
	// One byte over: the first maxExecCaptureBytes stay, the marker is appended.
	if _, err := b.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.HasPrefix(got, big) {
		t.Error("truncated output lost its prefix")
	}
	if !strings.HasSuffix(got, execTruncationMarker) {
		t.Errorf("truncated output missing marker: %q", got[len(got)-len(execTruncationMarker):])
	}
	if n := len(got) - len(execTruncationMarker); n != maxExecCaptureBytes {
		t.Errorf("captured %d bytes, want %d", n, maxExecCaptureBytes)
	}
	// Writes after truncation keep draining without growing the buffer.
	if _, err := b.Write([]byte("zzz")); err != nil {
		t.Fatal(err)
	}
	if n := len(b.String()) - len(execTruncationMarker); n != maxExecCaptureBytes {
		t.Errorf("buffer grew after truncation: %d", n)
	}
}

func TestLocalPortExecBoundedCapture(t *testing.T) {
	p, h := newLocalSandbox(t)
	ctx := context.Background()
	argv, err := p.ShellArgv("yes x | head -c 2000000")
	if err != nil {
		t.Skipf("no shell: %v", err)
	}
	res, err := p.Exec(ctx, h, argv)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(res.Stdout) != maxExecCaptureBytes+len(execTruncationMarker) {
		t.Errorf("stdout = %d bytes, want %d (bound + marker)", len(res.Stdout), maxExecCaptureBytes+len(execTruncationMarker))
	}
	if !strings.HasSuffix(res.Stdout, execTruncationMarker) {
		t.Error("runaway stdout missing truncation marker")
	}
	if !strings.HasPrefix(res.Stdout, strings.Repeat("x\n", 512)) {
		t.Error("runaway stdout lost its prefix")
	}
}

func TestLocalPortNetworkPolicyWarnsLoudly(t *testing.T) {
	ctx := context.Background()

	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	cases := []struct {
		name string
		opts Options
		warn bool
	}{
		{"empty mode stays quiet", Options{}, false},
		{"open stays quiet", Options{Network: NetworkPolicy{Mode: NetworkOpen}}, false},
		{"deny warns", Options{Network: NetworkPolicy{Mode: NetworkDeny}}, true},
		{"allowlist warns", Options{Network: NetworkPolicy{Mode: NetworkAllowlist, AllowedHosts: []string{"api.example.com"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewLocalPort(t.TempDir())
			if _, err := p.Create(ctx, "sess-"+strings.ReplaceAll(tc.name, " ", "-"), tc.opts); err != nil {
				t.Fatalf("create: %v", err)
			}
			out := buf.String()
			if tc.warn && !strings.Contains(out, "cannot enforce the network policy") {
				t.Errorf("expected a loud warning, got:\n%s", out)
			}
			if !tc.warn && strings.Contains(out, "cannot enforce the network policy") {
				t.Errorf("unexpected warning for %s:\n%s", tc.name, out)
			}
			buf.Reset()
		})
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

// countingDestroyPort wraps a LocalPort and counts Destroy calls, so a test
// can assert the Manager never destroys a local workspace on resume.
type countingDestroyPort struct {
	*LocalPort
	destroys int
}

func (p *countingDestroyPort) Destroy(ctx context.Context, h Handle) error {
	p.destroys++
	return p.LocalPort.Destroy(ctx, h)
}

// TestLocalManagerResumeKeepsWorkspaceFiles is the regression test for the
// local-backend data loss: Ensure used to destroy the stopped handle before
// recreating, which for the local backend removed <root>/<sessionID> — the
// directory that also holds the ImageStore's per-session image files and the
// agent's durable workspace — on EVERY resume inside the deferred-stop grace
// period. The local backend must resume by reusing the directory, never by
// destroying it.
func TestLocalManagerResumeKeepsWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	port := &countingDestroyPort{LocalPort: NewLocalPort(root)}
	mgr := NewManager(port)
	ctx := context.Background()

	h1, err := mgr.Ensure(ctx, "s1", Options{})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := port.WriteFile(ctx, h1, "notes/keep.txt", strings.NewReader("durable")); err != nil {
		t.Fatalf("write: %v", err)
	}

	mgr.MarkSessionEnded("s1", time.Hour)

	h2, err := mgr.Ensure(ctx, "s1", Options{})
	if err != nil {
		t.Fatalf("ensure after deferred stop: %v", err)
	}
	if h1.ID != h2.ID {
		t.Errorf("resume got a new handle %q, want the same %q (directory reused)", h2.ID, h1.ID)
	}
	if port.destroys != 0 {
		t.Errorf("resume destroyed the local workspace %d time(s); want 0", port.destroys)
	}

	// The durable file must still be readable through the resumed handle.
	rc, err := port.ReadFile(ctx, h2, "notes/keep.txt")
	if err != nil {
		t.Fatalf("workspace file lost on resume: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "durable" {
		t.Errorf("resumed file content = %q, want %q", b, "durable")
	}
}
