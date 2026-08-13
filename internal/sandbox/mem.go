package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemPort is an in-memory Port for tests and for developing consumers before a
// real backend is wired. Files live in a per-sandbox map; Exec echoes.
type MemPort struct {
	mu        sync.Mutex
	sandboxes map[string]*memSandbox
	nextID    int
}

type memSandbox struct {
	handle    Handle
	opts      Options
	files     map[string][]byte
	destroyed bool
}

// NewMemPort creates an empty in-memory Port.
func NewMemPort() *MemPort {
	return &MemPort{sandboxes: map[string]*memSandbox{}}
}

// Create starts an in-memory sandbox.
func (p *MemPort) Create(_ context.Context, sessionID string, opts Options) (Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	h := Handle{ID: fmt.Sprintf("mem-%d", p.nextID), SessionID: sessionID}
	p.sandboxes[h.ID] = &memSandbox{handle: h, opts: opts, files: map[string][]byte{}}
	return h, nil
}

// Destroy tears down the sandbox.
func (p *MemPort) Destroy(_ context.Context, h Handle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return fmt.Errorf("sandbox %s not found", h.ID)
	}
	sb.destroyed = true
	delete(p.sandboxes, h.ID)
	return nil
}

// Exec records nothing and returns a canned successful result.
func (p *MemPort) Exec(_ context.Context, h Handle, cmd []string) (ExecResult, error) {
	start := time.Now()
	if _, ok := p.sandboxes[h.ID]; !ok {
		return ExecResult{}, fmt.Errorf("sandbox %s not found", h.ID)
	}
	return ExecResult{ExitCode: 0, Stdout: "", Stderr: "", Duration: time.Since(start)}, nil
}

// ShellArgv wraps a POSIX script for a POSIX shell (Sheller capability).
func (p *MemPort) ShellArgv(script string) ([]string, error) {
	return []string{"sh", "-c", script}, nil
}

// ReadFile reads an in-memory file.
func (p *MemPort) ReadFile(_ context.Context, h Handle, path string) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", h.ID)
	}
	b, ok := sb.files[path]
	if !ok {
		return nil, fmt.Errorf("file %s not found", path)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// WriteFile writes an in-memory file.
func (p *MemPort) WriteFile(_ context.Context, h Handle, path string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return fmt.Errorf("sandbox %s not found", h.ID)
	}
	sb.files[path] = b
	return nil
}

// ListDir lists in-memory file paths.
func (p *MemPort) ListDir(_ context.Context, h Handle, _ string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", h.ID)
	}
	paths := make([]string, 0, len(sb.files))
	for k := range sb.files {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	return paths, nil
}

// Move renames/moves in-memory files. A directory move rewrites every key under
// the source prefix to the destination prefix (the flat key space has no real
// directories, so a "directory" is a key prefix).
func (p *MemPort) Move(_ context.Context, h Handle, src, dst string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return fmt.Errorf("sandbox %s not found", h.ID)
	}
	if _, isFile := sb.files[src]; !isFile && !hasPrefix(sb.files, src) {
		return fmt.Errorf("path %s not found", src)
	}
	if b, isFile := sb.files[src]; isFile {
		sb.files[dst] = b
		delete(sb.files, src)
	}
	for _, k := range keysUnder(sb.files, src) {
		rel := strings.TrimPrefix(k, src)
		sb.files[dst+rel] = sb.files[k]
		delete(sb.files, k)
	}
	return nil
}

// Copy duplicates in-memory files. A directory copy rewrites every key under the
// source prefix to the destination prefix (recursive).
func (p *MemPort) Copy(_ context.Context, h Handle, src, dst string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return fmt.Errorf("sandbox %s not found", h.ID)
	}
	if _, isFile := sb.files[src]; !isFile && !hasPrefix(sb.files, src) {
		return fmt.Errorf("path %s not found", src)
	}
	if b, isFile := sb.files[src]; isFile {
		cp := make([]byte, len(b))
		copy(cp, b)
		sb.files[dst] = cp
	}
	for _, k := range keysUnder(sb.files, src) {
		rel := strings.TrimPrefix(k, src)
		cp := make([]byte, len(sb.files[k]))
		copy(cp, sb.files[k])
		sb.files[dst+rel] = cp
	}
	return nil
}

// Delete removes an in-memory file, or a directory prefix and everything under
// it (recursive — the flat key space has no empty directories to leave behind).
func (p *MemPort) Delete(_ context.Context, h Handle, path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return fmt.Errorf("sandbox %s not found", h.ID)
	}
	if _, isFile := sb.files[path]; !isFile && !hasPrefix(sb.files, path) {
		return fmt.Errorf("path %s not found", path)
	}
	delete(sb.files, path)
	for _, k := range keysUnder(sb.files, path) {
		delete(sb.files, k)
	}
	return nil
}

// Mkdir is a no-op for the in-memory backend: directories are implicit in the
// flat key space (a file "a/b.txt" makes "a" exist). It only validates the
// sandbox is live so the Port contract holds uniformly.
func (p *MemPort) Mkdir(_ context.Context, h Handle, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sandboxes[h.ID]; !ok {
		return fmt.Errorf("sandbox %s not found", h.ID)
	}
	return nil
}

// hasPrefix reports whether any key sits under the directory prefix path.
func hasPrefix(files map[string][]byte, path string) bool {
	for k := range files {
		if strings.HasPrefix(k, path+"/") {
			return true
		}
	}
	return false
}

// keysUnder returns every key strictly under the directory prefix path.
func keysUnder(files map[string][]byte, path string) []string {
	var out []string
	for k := range files {
		if strings.HasPrefix(k, path+"/") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Walk lists in-memory file paths under root (Walker capability). Files are
// keyed by their workspace-relative forward-slash path already, so this filters
// the flat map by prefix.
func (p *MemPort) Walk(_ context.Context, h Handle, root string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.sandboxes[h.ID]
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", h.ID)
	}
	root = strings.Trim(strings.TrimPrefix(root, "./"), "/")
	out := make([]string, 0, len(sb.files))
	for k := range sb.files {
		if root == "" || root == "." || k == root || strings.HasPrefix(k, root+"/") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}
