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

// ResolveInterpreter picks a working interpreter on the host (InterpreterResolver
// capability). It probes with LookPath, but orders Python candidates to sidestep
// the Windows Store `python3` stub: that shim sits on PATH as a real executable
// yet exits nonzero doing nothing, so on Windows the `py` launcher and a real
// `python` are preferred over it. Other interpreters resolve in candidate order.
func (p *LocalPort) ResolveInterpreter(candidates []string) string {
	ordered := orderForHost(runtime.GOOS, candidates)
	for _, c := range ordered {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return ""
}

// orderForHost reorders interpreter candidates for the host OS. On Windows the
// Python launcher `py` and a real `python` come before `python3` (often the
// Store stub); elsewhere the conventional `python3`-first order is kept. Only
// Python names are reordered — anything else keeps its given priority.
func orderForHost(goos string, candidates []string) []string {
	if goos != "windows" {
		return candidates
	}
	rank := map[string]int{"py": 0, "python": 1, "python3": 2}
	out := append([]string(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, iOk := rank[out[i]]
		rj, jOk := rank[out[j]]
		if iOk && jOk {
			return ri < rj
		}
		// Ranked (Python) names sort ahead of unranked ones.
		return iOk
	})
	return out
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

	// Symlink-aware containment. If the target exists, resolve symlinks on both
	// the root and the file and require the resolved file to stay under the
	// resolved root. A non-existent target can't itself be a symlink yet, but
	// its parent chain can — a write to "link-outside/new.txt" must not land
	// outside the workspace — so resolve the nearest existing ancestor of the
	// parent and confine that instead.
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if _, statErr := os.Lstat(full); statErr == nil {
		resolvedFull, err := filepath.EvalSymlinks(fullAbs)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %w", path, err)
		}
		if !within(resolvedRoot, resolvedFull) {
			return "", fmt.Errorf("path %q escapes the workspace via symlink", path)
		}
	} else {
		resolvedParent, err := resolveNearestExisting(filepath.Dir(fullAbs))
		if err != nil {
			return "", fmt.Errorf("resolve parent of %q: %w", path, err)
		}
		if !within(resolvedRoot, resolvedParent) {
			return "", fmt.Errorf("path %q escapes the workspace via symlinked parent", path)
		}
	}
	return full, nil
}

// resolveNearestExisting evaluates symlinks on the deepest existing ancestor
// of path, climbing up until EvalSymlinks succeeds. It is used to confine
// writes into not-yet-created paths whose parent chain may contain a symlink
// pointing outside the workspace.
func resolveNearestExisting(path string) (string, error) {
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return resolved, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
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
	var stdout, stderr boundedCapture
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

// Move renames/moves a workspace-confined file or directory. Both paths are
// resolved (and thus confined) like every other workspace path.
func (p *LocalPort) Move(_ context.Context, h Handle, src, dst string) error {
	srcFull, err := p.resolve(h, src)
	if err != nil {
		return err
	}
	dstFull, err := p.resolve(h, dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstFull), 0o755); err != nil {
		return fmt.Errorf("prepare parent dir: %w", err)
	}
	if err := os.Rename(srcFull, dstFull); err != nil {
		return fmt.Errorf("move %q to %q: %w", src, dst, err)
	}
	return nil
}

// Copy duplicates a workspace-confined file or directory (recursively). Both
// paths are resolved (and thus confined) like every other workspace path.
func (p *LocalPort) Copy(_ context.Context, h Handle, src, dst string) error {
	srcFull, err := p.resolve(h, src)
	if err != nil {
		return err
	}
	dstFull, err := p.resolve(h, dst)
	if err != nil {
		return err
	}
	info, err := os.Stat(srcFull)
	if err != nil {
		return fmt.Errorf("copy source %q: %w", src, err)
	}
	if info.IsDir() {
		return copyDir(srcFull, dstFull)
	}
	return copyFile(srcFull, dstFull)
}

// copyFile copies a single file, creating the destination's parent directory.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("prepare parent dir: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyDir recursively copies a directory tree, preserving relative structure.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(p, target)
	})
}

// Delete removes a workspace-confined file or directory (recursively). The path
// is resolved (and thus confined) like every other workspace path.
func (p *LocalPort) Delete(_ context.Context, h Handle, path string) error {
	full, err := p.resolve(h, path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(full); err != nil {
		return fmt.Errorf("delete %q: %w", path, err)
	}
	return nil
}

// Mkdir creates a workspace-confined directory (and any parents). The path is
// resolved (and thus confined) like every other workspace path.
func (p *LocalPort) Mkdir(_ context.Context, h Handle, path string) error {
	full, err := p.resolve(h, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", path, err)
	}
	return nil
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
