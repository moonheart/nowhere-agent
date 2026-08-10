package sandbox

import (
	"context"
	"io"
	"strconv"
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
	// Allowlist must fail closed (no egress) until an enforcing proxy exists,
	// never fall open to bridge.
	allow, err := dockerNetworkMode(NetworkPolicy{Mode: NetworkAllowlist})
	if err != nil || allow != "none" {
		t.Errorf("allowlist -> %q err %v, want none (fail closed)", allow, err)
	}
	// An unset policy also fails closed rather than opening egress.
	empty, err := dockerNetworkMode(NetworkPolicy{})
	if err != nil || empty != "none" {
		t.Errorf("empty -> %q err %v, want none (fail closed)", empty, err)
	}
	if _, err := dockerNetworkMode(NetworkPolicy{Mode: "bogus"}); err == nil {
		t.Error("expected error for unknown mode")
	}
}

// TestDockerHostConfigHardening verifies each session container gets resource
// limits and privilege drops so one tenant can't exhaust or escape the host.
// It builds the HostConfig only (no daemon needed).
func TestDockerHostConfigHardening(t *testing.T) {
	p := &DockerPort{image: "alpine:latest", workMt: "/workspace"}
	hc, err := p.hostConfig(Options{WorkspaceDir: "/tmp/ws", Network: NetworkPolicy{Mode: NetworkDeny}})
	if err != nil {
		t.Fatal(err)
	}
	if hc.NetworkMode != "none" {
		t.Errorf("network mode = %q want none", hc.NetworkMode)
	}
	if hc.Memory <= 0 {
		t.Error("memory limit not set")
	}
	if hc.NanoCPUs <= 0 {
		t.Error("cpu limit not set")
	}
	if hc.PidsLimit == nil || *hc.PidsLimit <= 0 {
		t.Error("pids limit not set (fork-bomb guard)")
	}
	if !hasString([]string(hc.CapDrop), "ALL") {
		t.Errorf("CapDrop = %v, want it to contain ALL", hc.CapDrop)
	}
	if !hasString([]string(hc.SecurityOpt), "no-new-privileges") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", hc.SecurityOpt)
	}
	if !hc.ReadonlyRootfs {
		t.Error("rootfs should be read-only")
	}
	if _, ok := hc.Tmpfs["/tmp"]; !ok {
		t.Error("expected a writable tmpfs /tmp alongside the read-only rootfs")
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Source != "/tmp/ws" {
		t.Errorf("workspace mount = %+v, want a single bind mount of /tmp/ws", hc.Mounts)
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
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

	// This test targets LINUX containers (alpine image, /workspace paths, a
	// Linux-only HostConfig like the read-only rootfs). A daemon serving
	// Windows containers (Docker Desktop on Windows CI) cannot run it — skip
	// rather than fail on "read-only mode is not supported for Windows
	// containers".
	if ver, err := p.cli.ServerVersion(ctx); err == nil && strings.EqualFold(ver.Os, "windows") {
		t.Skipf("docker daemon serves %s containers; the integration test targets linux", ver.Os)
	}

	wsDir := t.TempDir()
	h, err := p.Create(ctx, "itest-session", Options{WorkspaceDir: wsDir, Network: NetworkPolicy{Mode: NetworkDeny}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer p.Destroy(ctx, h)

	// The container must run as the workspace's owner uid, so exec-side
	// operations on the bind mount (parent-dir mkdir, ls, find) work under
	// user-namespace-remapped Docker (e.g. GitHub runners), where the remapped
	// container user cannot touch a workspace it does not own. Skipped when
	// there is no meaningful uid to map (Windows host, root-owned workspace).
	if uid, ok := workspaceOwnerUID(wsDir); ok && uid > 0 {
		inspect, err := p.cli.ContainerInspect(ctx, h.ID)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		if want := strconv.Itoa(uid); inspect.Config.User != want {
			t.Errorf("container user = %q, want %q (workspace owner)", inspect.Config.User, want)
		}
	}

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

	// Nested write: the parent directory does not exist yet, so WriteFile must
	// create it (the "creates parent directories" contract the spill relies on).
	if err := p.WriteFile(ctx, h, "/workspace/.nowhere/tool-results/x.txt", strings.NewReader("spilled")); err != nil {
		t.Fatalf("nested WriteFile: %v", err)
	}
	rc2, err := p.ReadFile(ctx, h, "/workspace/.nowhere/tool-results/x.txt")
	if err != nil {
		t.Fatalf("nested ReadFile: %v", err)
	}
	nb, _ := io.ReadAll(rc2)
	rc2.Close()
	if string(nb) != "spilled" {
		t.Errorf("nested read %q want spilled", nb)
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
