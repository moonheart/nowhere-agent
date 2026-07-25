package sandbox

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// LocalPort is a Port backed by a per-session host directory (design file-tools
// D4). It is the default backend for development and single-tenant self-hosting
// where a container runtime is unavailable. Files are confined to the session
// workspace by resolve(); the network policy is a no-op here (egress control
// requires a container/proxy layer — see the Docker backend and task 16.1).
type LocalPort struct {
	root  string
	shell string // optional bash override (SANDBOX_SHELL); empty = auto-detect
}

// NewLocalPort creates a local fs backend rooted at root (created on demand).
func NewLocalPort(root string) *LocalPort {
	return &LocalPort{root: root}
}

// WithShell sets the bash executable used by run_command (Sheller). Empty keeps
// auto-detection (bash on PATH; Git Bash on Windows). Chainable.
func (p *LocalPort) WithShell(shell string) *LocalPort {
	p.shell = shell
	return p
}

// ShellArgv wraps a POSIX script for the host shell (Sheller capability): bash
// on Linux/mac, Git Bash on Windows, honouring the configured override.
func (p *LocalPort) ShellArgv(script string) ([]string, error) {
	return shellArgv(runtime.GOOS, p.shell, script)
}

// Create makes the session workspace directory and returns its handle. When
// opts.WorkspaceDir is set it is used verbatim; otherwise the workspace is
// <root>/<sessionID>. Idempotent: re-creating an existing workspace keeps its
// files (the workspace is the durable state).
func (p *LocalPort) Create(_ context.Context, sessionID string, opts Options) (Handle, error) {
	dir := opts.WorkspaceDir
	if dir == "" {
		if p.root == "" {
			return Handle{}, fmt.Errorf("local sandbox root not configured")
		}
		dir = filepath.Join(p.root, sessionID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Handle{}, fmt.Errorf("create workspace: %w", err)
	}
	return Handle{ID: "local-" + sessionID, SessionID: sessionID}, nil
}

// Destroy removes the session workspace directory.
func (p *LocalPort) Destroy(_ context.Context, h Handle) error {
	dir, err := p.workspaceDir(h)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("destroy workspace: %w", err)
	}
	return nil
}

// workspaceDir returns the session's workspace root for a handle.
func (p *LocalPort) workspaceDir(h Handle) (string, error) {
	if p.root == "" {
		return "", fmt.Errorf("local sandbox root not configured")
	}
	return filepath.Join(p.root, h.SessionID), nil
}

// resolve maps a caller-supplied path to a path inside the session workspace,
// rejecting any escape (design file-tools D3). It rejects absolute paths and
// ".." that climbs above the root, then — for paths that already exist — uses
// EvalSymlinks so a symlink planted inside the workspace cannot point outside
// it. The confinement is structural (prefix of the resolved absolute path), not
// a string check.
func (p *LocalPort) resolve(h Handle, path string) (string, error) {
	if path == "" {
		path = "."
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute path %q escapes the workspace", path)
	}
	root, err := p.workspaceDir(h)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", path)
	}
	full := filepath.Join(root, clean)

	// Containment on the cleaned path (cheap, always applied).
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if !within(rootAbs, fullAbs) {
		return "", fmt.Errorf("path %q escapes the workspace", path)
	}

	// Symlink-aware containment: if the target exists, resolve symlinks on both
	// the root and the file and require the resolved file to stay under the
	// resolved root. A non-existent path (a write to a new file) can't be a
	// symlink yet, so the cleaned-path check above suffices.
	if _, statErr := os.Lstat(full); statErr == nil {
		resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
		if err != nil {
			return "", fmt.Errorf("resolve workspace: %w", err)
		}
		resolvedFull, err := filepath.EvalSymlinks(fullAbs)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %w", path, err)
		}
		if !within(resolvedRoot, resolvedFull) {
			return "", fmt.Errorf("path %q escapes the workspace via symlink", path)
		}
	}
	return full, nil
}

// within reports whether child is root itself or under it.
func within(root, child string) bool {
	if root == child {
		return true
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Exec runs a command with its working directory set to the session workspace.
// This completes the Port interface for the local backend; the file tools do
// not use it.
func (p *LocalPort) Exec(ctx context.Context, h Handle, argv []string) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, fmt.Errorf("empty command")
	}
	dir, err := p.workspaceDir(h)
	if err != nil {
		return ExecResult{}, err
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	res := ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
	if runErr != nil {
		if exit, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = exit.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("exec: %w", runErr)
	}
	return res, nil
}

// ReadFile reads a workspace-confined file.
func (p *LocalPort) ReadFile(_ context.Context, h Handle, path string) (io.ReadCloser, error) {
	full, err := p.resolve(h, path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	return f, nil
}

// WriteFile writes a workspace-confined file, creating parent directories.
func (p *LocalPort) WriteFile(_ context.Context, h Handle, path string, r io.Reader) error {
	full, err := p.resolve(h, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("prepare parent dir: %w", err)
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

// ListDir lists entry names under a workspace-confined directory.
func (p *LocalPort) ListDir(_ context.Context, h Handle, path string) ([]string, error) {
	full, err := p.resolve(h, path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", path, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// Walk lists every file under root recursively, as workspace-relative
// forward-slash paths (Walker capability). Directories and the .git tree are
// skipped. root is confined by resolve() like every other path.
func (p *LocalPort) Walk(_ context.Context, h Handle, root string) ([]string, error) {
	base, err := p.resolve(h, root)
	if err != nil {
		return nil, err
	}
	wsRoot, err := p.workspaceDir(h)
	if err != nil {
		return nil, err
	}
	var out []string
	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(wsRoot, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %q: %w", root, walkErr)
	}
	sort.Strings(out)
	return out, nil
}
