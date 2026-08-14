// Package sandbox implements the sandbox capability (design D3): per-session
// isolation behind a SandboxPort. The built-in implementation uses filesystem
// isolation plus Docker; the interface hides local-vs-remote so gVisor or
// Firecracker backends can be added without changing consumers.
package sandbox

import (
	"context"
	"io"
	"strings"
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

// maxExecCaptureBytes bounds each Exec output stream (stdout/stderr) so a
// runaway command cannot balloon the gateway's memory before the tool layer's
// spill cap (builtin capAndSpill) ever sees the result: run_command allows a
// 120s budget, during which `yes` or `cat /dev/zero` would otherwise push the
// process into gigabytes. Both Port backends (local, docker) capture through
// this same bound.
const maxExecCaptureBytes = 1 << 20 // 1 MiB per stream

// execTruncationMarker is appended to a stream that exceeded the capture
// bound, so consumers can tell truncation apart from a complete output.
const execTruncationMarker = "\n… [output truncated: exceeded 1 MiB capture limit]\n"

// boundedCapture accumulates at most maxExecCaptureBytes of a stream, then
// keeps discarding and appends execTruncationMarker to String(). Bytes within
// the bound are returned verbatim; Write always reports the full length so
// exec/stdcopy keep draining without erroring.
type boundedCapture struct {
	buf       strings.Builder
	truncated bool
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	if !b.truncated {
		remaining := maxExecCaptureBytes - b.buf.Len()
		if remaining <= 0 {
			b.truncated = true
		} else if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedCapture) String() string {
	s := b.buf.String()
	if b.truncated {
		s += execTruncationMarker
	}
	return s
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

	// Move renames/moves a file or directory within the sandbox. Both paths are
	// workspace-relative and confined like any other (no escape).
	Move(ctx context.Context, h Handle, src, dst string) error

	// Copy duplicates a file or directory (recursively) within the sandbox. Both
	// paths are workspace-relative and confined like any other.
	Copy(ctx context.Context, h Handle, src, dst string) error

	// Delete removes a file or directory (recursively) within the sandbox. The
	// path is workspace-relative and confined like any other.
	Delete(ctx context.Context, h Handle, path string) error

	// Mkdir creates a directory (and any parents) within the sandbox. The path is
	// workspace-relative and confined like any other.
	Mkdir(ctx context.Context, h Handle, path string) error
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

// Sheller is an optional Port capability: wrap a POSIX shell script into the
// argv that runs it under the backend's shell. It backs the run_command tool.
// The command contract is uniform bash/POSIX across backends — docker runs it in
// the Linux container's sh; the local backend runs it under bash (Git Bash on
// Windows) — so a model writes one shell dialect regardless of host OS. A Port
// that does not implement it does not offer run_command.
type Sheller interface {
	ShellArgv(script string) ([]string, error)
}

// InterpreterResolver is an optional Port capability: pick a working interpreter
// for a script from a candidate list, for the backend's own execution
// environment. The skill ScriptTool calls it so a `.py` script runs under an
// interpreter that actually exists where the command will run — the local
// backend probes the host (and must sidestep the Windows Store `python3` stub),
// while the docker backend probes the container with `command -v`. A Port that
// does not implement it leaves the tool to use the first candidate.
type InterpreterResolver interface {
	// ResolveInterpreter returns the first candidate usable in this backend, or
	// "" if none are. Candidates are bare executable names ("python3", "node").
	ResolveInterpreter(ctx context.Context, h Handle, candidates []string) string
}
