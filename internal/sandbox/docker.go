package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerPort is the built-in Port backed by Docker containers (design D3).
// Each session gets one container; the workspace dir is bind-mounted in; the
// network policy maps to Docker network settings at creation.
type DockerPort struct {
	cli    *client.Client
	image  string
	workMt string // container path the workspace is mounted at
}

// Per-container resource limits so one tenant cannot exhaust the shared host
// (memory, CPU, or PID/fork-bomb). Conservative defaults sized for the built-in
// file tools and short commands.
const (
	defaultMemoryLimitBytes = 512 * 1024 * 1024 // 512 MiB
	defaultCPULimitNano     = 1_000_000_000     // 1 CPU
	defaultPidsLimit        = 256
)

// DockerOption customizes DockerPort.
type DockerOption func(*DockerPort)

// WithImage sets the container image.
func WithImage(img string) DockerOption { return func(p *DockerPort) { p.image = img } }

// NewDockerPort creates a Docker-backed Port using the environment's Docker.
func NewDockerPort(opts ...DockerOption) (*DockerPort, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	p := &DockerPort{cli: cli, image: "alpine:latest", workMt: "/workspace"}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// Create starts a container for the session with the workspace mounted and the
// network policy applied.
func (p *DockerPort) Create(ctx context.Context, sessionID string, opts Options) (Handle, error) {
	img := opts.Image
	if img == "" {
		img = p.image
	}
	if err := p.ensureImage(ctx, img); err != nil {
		return Handle{}, err
	}

	cfg := &container.Config{
		Image:      img,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: p.workMt,
	}
	hostCfg, err := p.hostConfig(opts)
	if err != nil {
		return Handle{}, err
	}

	name := "nowhere-" + sanitizeName(sessionID)
	resp, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return Handle{}, fmt.Errorf("container create: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return Handle{}, fmt.Errorf("container start: %w", err)
	}
	return Handle{ID: resp.ID, SessionID: sessionID}, nil
}

// hostConfig builds the container HostConfig: the egress policy, the workspace
// bind mount, and the isolation hardening — resource limits (memory/CPU/PID),
// all Linux capabilities dropped, no privilege escalation, and a read-only
// rootfs with a writable tmpfs /tmp. The file tools (Docker copy API) and the
// internal `ls` need none of the dropped privileges. (Running as a non-root
// UID is a further step, deferred until workspace-mount UID mapping is handled.)
func (p *DockerPort) hostConfig(opts Options) (*container.HostConfig, error) {
	netMode, err := dockerNetworkMode(opts.Network)
	if err != nil {
		return nil, err
	}
	pids := int64(defaultPidsLimit)
	hostCfg := &container.HostConfig{
		NetworkMode: netMode,
		Resources: container.Resources{
			Memory:    defaultMemoryLimitBytes,
			NanoCPUs:  defaultCPULimitNano,
			PidsLimit: &pids,
		},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": ""},
	}
	if opts.WorkspaceDir != "" {
		hostCfg.Mounts = []mount.Mount{{
			Type:   mount.TypeBind,
			Source: opts.WorkspaceDir,
			Target: p.workMt,
		}}
	}
	return hostCfg, nil
}

// Destroy stops and removes the container.
func (p *DockerPort) Destroy(ctx context.Context, h Handle) error {
	timeout := 5
	_ = p.cli.ContainerStop(ctx, h.ID, container.StopOptions{Timeout: &timeout})
	if err := p.cli.ContainerRemove(ctx, h.ID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("container remove: %w", err)
	}
	return nil
}

// ShellArgv wraps a POSIX script for the container shell (Sheller capability).
// The container is Linux, so `sh -c` runs the script regardless of host OS.
func (p *DockerPort) ShellArgv(script string) ([]string, error) {
	return []string{"sh", "-c", script}, nil
}

// Exec runs a command in the container and captures output.
func (p *DockerPort) Exec(ctx context.Context, h Handle, cmd []string) (ExecResult, error) {
	start := time.Now()
	exec, err := p.cli.ContainerExecCreate(ctx, h.ID, container.ExecOptions{
		Cmd:          cmd,
		WorkingDir:   p.workMt,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	resp, err := p.cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer resp.Close()

	stdout, stderr, err := demuxDockerStream(resp.Reader)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec read: %w", err)
	}

	inspect, err := p.cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec inspect: %w", err)
	}
	return ExecResult{
		ExitCode: inspect.ExitCode,
		Stdout:   stdout,
		Stderr:   stderr,
		Duration: time.Since(start),
	}, nil
}

// ReadFile reads a file from the container via a tar stream.
func (p *DockerPort) ReadFile(ctx context.Context, h Handle, path string) (io.ReadCloser, error) {
	rc, _, err := p.cli.CopyFromContainer(ctx, h.ID, path)
	if err != nil {
		return nil, fmt.Errorf("copy from container: %w", err)
	}
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		rc.Close()
		return nil, fmt.Errorf("read tar: %w", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, tr); err != nil {
		rc.Close()
		return nil, err
	}
	rc.Close()
	return io.NopCloser(&buf), nil
}

// WriteFile writes a file into the container via a tar stream.
func (p *DockerPort) WriteFile(ctx context.Context, h Handle, path string, r io.Reader) error {
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: filepath.Base(path),
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(content); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := p.cli.CopyToContainer(ctx, h.ID, dir, &buf, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	return nil
}

// ListDir lists entries under path in the container.
func (p *DockerPort) ListDir(ctx context.Context, h Handle, path string) ([]string, error) {
	res, err := p.Exec(ctx, h, []string{"ls", "-1", path})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("ls failed: %s", res.Stderr)
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// Walk lists every file under root recursively (Walker capability), as
// workspace-relative forward-slash paths. Exec runs with WorkingDir set to the
// workspace mount, so a relative root and the returned paths are workspace-
// relative (the leading "./" that find emits is stripped).
func (p *DockerPort) Walk(ctx context.Context, h Handle, root string) ([]string, error) {
	if root == "" {
		root = "."
	}
	res, err := p.Exec(ctx, h, []string{"find", root, "-type", "f"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("find failed: %s", res.Stderr)
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, strings.TrimPrefix(line, "./"))
		}
	}
	return out, nil
}

// ensureImage pulls the image if not present locally.
func (p *DockerPort) ensureImage(ctx context.Context, img string) error {
	_, _, err := p.cli.ImageInspectWithRaw(ctx, img)
	if err == nil {
		return nil
	}
	rc, err := p.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// dockerNetworkMode maps a NetworkPolicy to a Docker network mode.
func dockerNetworkMode(np NetworkPolicy) (container.NetworkMode, error) {
	switch np.Mode {
	case NetworkOpen:
		return "bridge", nil
	case NetworkDeny, "":
		return "none", nil
	case NetworkAllowlist:
		// An allowlist requires an egress proxy that enforces AllowedHosts
		// (TODO D3). Until that proxy network exists we FAIL CLOSED — "none", no
		// egress — rather than "bridge", which would silently grant full egress
		// and defeat the policy. A denied request is safe; an unexpectedly-open
		// one is a security hole.
		return "none", nil
	default:
		return "", fmt.Errorf("unknown network mode %q", np.Mode)
	}
}

// sanitizeName makes a sessionID safe for a container name.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// demuxDockerStream splits Docker's multiplexed stdout/stderr stream.
func demuxDockerStream(r io.Reader) (string, string, error) {
	var stdout, stderr bytes.Buffer
	_, err := stdcopy.StdCopy(&stdout, &stderr, r)
	return stdout.String(), stderr.String(), err
}
