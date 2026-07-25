package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"nowhere-agent/internal/sandbox"
)

// fakeExecPort implements sandbox.Sheller and Exec, recording the argv it was
// asked to run and returning a canned result. It embeds the Port interface (nil)
// for the methods run_command never calls.
type fakeExecPort struct {
	sandbox.Port
	gotArgv []string
	result  sandbox.ExecResult
	execErr error
}

func (f *fakeExecPort) ShellArgv(script string) ([]string, error) {
	return []string{"bash", "-c", script}, nil
}

func (f *fakeExecPort) Exec(_ context.Context, _ sandbox.Handle, argv []string) (sandbox.ExecResult, error) {
	f.gotArgv = argv
	return f.result, f.execErr
}

func TestRunCommandSuccess(t *testing.T) {
	fp := &fakeExecPort{result: sandbox.ExecResult{Stdout: "hello\n", ExitCode: 0}}
	res, err := NewRunCommand(fp, sandbox.Handle{}).Call(context.Background(), map[string]any{"command": "echo hello"})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v", res)
	}
	if res.Content != "hello" {
		t.Errorf("content = %q", res.Content)
	}
	if len(fp.gotArgv) != 3 || fp.gotArgv[0] != "bash" || fp.gotArgv[2] != "echo hello" {
		t.Errorf("argv = %v", fp.gotArgv)
	}
}

func TestRunCommandNonZeroExitNotError(t *testing.T) {
	fp := &fakeExecPort{result: sandbox.ExecResult{Stderr: "boom\n", ExitCode: 2}}
	res, _ := NewRunCommand(fp, sandbox.Handle{}).Call(context.Background(), map[string]any{"command": "false"})
	if res.IsError {
		t.Error("a non-zero exit must NOT be reported as a tool error")
	}
	if !strings.Contains(res.Content, "[stderr]") || !strings.Contains(res.Content, "boom") || !strings.Contains(res.Content, "[exit code 2]") {
		t.Errorf("content = %q", res.Content)
	}
}

func TestRunCommandInfraErrorIsError(t *testing.T) {
	fp := &fakeExecPort{execErr: fmt.Errorf("daemon down")}
	res, _ := NewRunCommand(fp, sandbox.Handle{}).Call(context.Background(), map[string]any{"command": "x"})
	if !res.IsError || !strings.Contains(res.Content, "daemon down") {
		t.Errorf("want infra error, got %+v", res)
	}
}

func TestRunCommandUnsupportedBackend(t *testing.T) {
	// noWalkPort (defined in search_test.go) embeds the Port interface only, so it
	// implements neither Walker nor Sheller — exercising the graceful path.
	sb := noWalkPort{sandbox.NewMemPort()}
	res, _ := NewRunCommand(sb, sandbox.Handle{}).Call(context.Background(), map[string]any{"command": "x"})
	if !res.IsError || !strings.Contains(res.Content, "not supported") {
		t.Errorf("want not-supported, got %+v", res)
	}
}

func TestRunCommandMissingArg(t *testing.T) {
	fp := &fakeExecPort{}
	res, _ := NewRunCommand(fp, sandbox.Handle{}).Call(context.Background(), map[string]any{})
	if !res.IsError || !strings.Contains(res.Content, "command") {
		t.Errorf("want missing-command error, got %+v", res)
	}
}

func TestFormatExec(t *testing.T) {
	if got := formatExec(sandbox.ExecResult{}); got != "(no output)" {
		t.Errorf("empty = %q", got)
	}
	if got := formatExec(sandbox.ExecResult{Stdout: "a\n"}); got != "a" {
		t.Errorf("stdout-only = %q", got)
	}
	got := formatExec(sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 1})
	if !strings.Contains(got, "out") || !strings.Contains(got, "[stderr]\nerr") || !strings.Contains(got, "[exit code 1]") {
		t.Errorf("combined = %q", got)
	}
}

func TestFormatExecTruncates(t *testing.T) {
	big := strings.Repeat("x", runCommandMaxOutput+100)
	got := formatExec(sandbox.ExecResult{Stdout: big})
	if !strings.Contains(got, "truncated at") {
		t.Error("expected a truncation note")
	}
	if len(got) > runCommandMaxOutput+80 {
		t.Errorf("output not capped: len=%d", len(got))
	}
}
