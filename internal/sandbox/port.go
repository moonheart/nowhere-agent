// Package sandbox implements the sandbox capability (design D3): per-session
// isolation behind a SandboxPort. The built-in implementation uses filesystem
// isolation plus Docker; the interface hides local-vs-remote so gVisor or
// Firecracker backends can be added without changing consumers.
package sandbox

import (
	"context"
	"io"
	"time"
)

// NetworkMode controls egress from inside the sandbox.
type NetworkMode string

const (
	// NetworkOpen allows all outbound traffic.
	NetworkOpen NetworkMode = "open"
	// NetworkAllowlist allows only AllowedHosts.
	NetworkAllowlist NetworkMode = "allowlist"
	// NetworkDeny blocks all outbound traffic.
	NetworkDeny NetworkMode = "deny"
)

// NetworkPolicy is enforced at the container layer (egress proxy/firewall),
// set at Create time. This is what makes execution-permission's network gate
// real — Go-level checks cannot stop code inside from dialing out.
type NetworkPolicy struct {
	Mode         NetworkMode
	AllowedHosts []string
}

// Options configure a new sandbox.
type Options struct {
	// WorkspaceDir is the host directory mounted as the session workspace.
	WorkspaceDir string
	// Network is the egress policy applied at creation.
	Network NetworkPolicy
	// Image overrides the container image (Docker impl); empty uses default.
	Image string
}

// Handle identifies a live sandbox.
type Handle struct {
	ID        string
	SessionID string
}

// ExecResult is the outcome of running a command.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// Port is the minimal verb set the agent loop and skill engine use. Consumers
// operate identically regardless of the backing implementation.
type Port interface {
	// Create starts a sandbox for a session.
	Create(ctx context.Context, sessionID string, opts Options) (Handle, error)

	// Destroy tears down a sandbox and releases its resources.
	Destroy(ctx context.Context, h Handle) error

	// Exec runs a command inside the sandbox.
	Exec(ctx context.Context, h Handle, cmd []string) (ExecResult, error)

	// ReadFile reads a file from the sandbox filesystem.
	ReadFile(ctx context.Context, h Handle, path string) (io.ReadCloser, error)

	// WriteFile writes a file into the sandbox filesystem.
	WriteFile(ctx context.Context, h Handle, path string, r io.Reader) error

	// ListDir lists entries under path in the sandbox.
	ListDir(ctx context.Context, h Handle, path string) ([]string, error)
}

// Walker is an optional Port capability: list every file under root recursively,
// as workspace-relative forward-slash paths. It backs the grep and glob tools,
// which need a whole-tree view the one-level ListDir cannot give. A Port that
// does not implement it simply does not offer those tools (they report an
// is_error result rather than crashing). The three built-in backends (local,
// docker, mem) all implement it.
type Walker interface {
	Walk(ctx context.Context, h Handle, root string) ([]string, error)
}
