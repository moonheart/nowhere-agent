package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abc-123_X", "abc-123_X"},
		{"a/b.c", "a-b-c"},
		{"session:1", "session-1"},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.in); got != tt.want {
			t.Errorf("sanitizeName(%q) = %q want %q", tt.in, got, tt.want)
		}
	}
}

func TestDockerNetworkMode(t *testing.T) {
	open, err := dockerNetworkMode(NetworkPolicy{Mode: NetworkOpen})
	if err != nil || open != "bridge" {
		t.Errorf("open -> %q err %v", open, err)
	}
	deny, err := dockerNetworkMode(NetworkPolicy{Mode: NetworkDeny})
	if err != nil || deny != "none" {
		t.Errorf("deny -> %q err %v", deny, err)
	}
	if _, err := dockerNetworkMode(NetworkPolicy{Mode: "bogus"}); err == nil {
		t.Error("expected error for unknown mode")
	}
}

// TestDockerPortIntegration exercises the real Docker backend end to end.
// Skipped when no Docker daemon is reachable.
func TestDockerPortIntegration(t *testing.T) {
	p, err := NewDockerPort()
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Probe daemon.
	if _, err := p.cli.Ping(ctx); err != nil {
		p.cli.Close()
		t.Skipf("no docker daemon: %v", err)
	}
	defer p.cli.Close()

	wsDir := t.TempDir()
	h, err := p.Create(ctx, "itest-session", Options{WorkspaceDir: wsDir, Network: NetworkPolicy{Mode: NetworkDeny}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer p.Destroy(ctx, h)

	// Exec.
	res, err := p.Exec(ctx, h, []string{"sh", "-c", "echo hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d", res.ExitCode)
	}

	// Write + read back through the workspace mount.
	if err := p.WriteFile(ctx, h, "/workspace/note.txt", strings.NewReader("persisted")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := p.ReadFile(ctx, h, "/workspace/note.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "persisted" {
		t.Errorf("read %q want persisted", b)
	}

	// List.
	entries, err := p.ListDir(ctx, h, "/workspace")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e == "note.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("note.txt not in listing %v", entries)
	}
}
