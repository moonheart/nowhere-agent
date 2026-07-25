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
	handle  Handle
	opts    Options
	files   map[string][]byte
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
