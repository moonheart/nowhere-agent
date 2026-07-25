package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/toolruntime"
)

const runCommandMaxOutput = 20_000

// NewRunCommand returns the run_command tool bound to a session sandbox. It runs
// a POSIX/bash script inside the sandbox via the Port's Sheller + Exec, capturing
// stdout, stderr, and the exit code. The command contract is uniform bash across
// backends (container sh on docker, Git Bash on Windows local, bash/sh on unix
// local), so the model writes one shell dialect everywhere.
//
// Risk is SandboxWrite: on the docker backend the command is confined to the
// container; on the local backend it runs on the host with the workspace as its
// working directory, so local exec is a trusted single-tenant mode (gated by
// SANDBOX_LOCAL_EXEC) and multi-tenant deployments should use the docker backend.
func NewRunCommand(sb sandbox.Port, h sandbox.Handle) toolruntime.Tool {
	return &runCommandTool{sb: sb, h: h}
}

type runCommandTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *runCommandTool) Name() string { return "run_command" }
func (t *runCommandTool) Description() string {
	return "Run a shell command (POSIX/bash) in the session workspace and return its stdout, stderr, and exit code. Use for builds, tests, git, and other tooling. A non-zero exit code is reported as normal output, not a tool error."
}
func (t *runCommandTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "shell script to run (bash/POSIX), executed with the workspace as the working directory"},
		},
		"required": []string{"command"},
	}
}
func (t *runCommandTool) Risk() toolruntime.Risk { return toolruntime.RiskSandboxWrite }
func (t *runCommandTool) Timeout() time.Duration { return 120 * time.Second }

func (t *runCommandTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	command, err := argString(args, "command")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	sh, ok := t.sb.(sandbox.Sheller)
	if !ok {
		return toolruntime.Result{Content: "run_command is not supported by this sandbox backend", IsError: true}, nil
	}
	argv, err := sh.ShellArgv(command)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("run_command failed: %v", err), IsError: true}, nil
	}
	res, err := t.sb.Exec(ctx, t.h, argv)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("run_command failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: formatExec(res)}, nil
}

// formatExec renders an ExecResult for the model: stdout, then stderr (labelled)
// if any, then the exit code when non-zero. A non-zero exit is normal output —
// the model wants to see the failure — not an is_error. Output is capped so a
// runaway command cannot blow the context window.
func formatExec(res sandbox.ExecResult) string {
	var b strings.Builder
	if stdout := strings.TrimRight(res.Stdout, "\n"); stdout != "" {
		b.WriteString(stdout)
	}
	if stderr := strings.TrimRight(res.Stderr, "\n"); stderr != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[stderr]\n")
		b.WriteString(stderr)
	}
	if res.ExitCode != 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(fmt.Sprintf("[exit code %d]", res.ExitCode))
	}
	out := b.String()
	if out == "" {
		return "(no output)"
	}
	if len(out) > runCommandMaxOutput {
		out = out[:runCommandMaxOutput] + fmt.Sprintf("\n… [output truncated at %d bytes]", runCommandMaxOutput)
	}
	return out
}
