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

func TestFormatExecReturnsFullOutput(t *testing.T) {
	// formatExec no longer caps — capAndSpill (in Call) bounds the result now, so
	// the formatter returns the full output verbatim.
	big := strings.Repeat("x", spillCap+100)
	if got := formatExec(sandbox.ExecResult{Stdout: big}); got != big {
		t.Errorf("formatExec should return the full output uncapped; len=%d want %d", len(got), len(big))
	}
}

// bigExecPort embeds a real MemPort (for ShellArgv + workspace storage) but
// overrides Exec to return a canned oversized result, so run_command's spill
// path can be exercised end to end.
type bigExecPort struct {
	*sandbox.MemPort
	out string
}

func (p *bigExecPort) Exec(_ context.Context, _ sandbox.Handle, _ []string) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{Stdout: p.out, ExitCode: 0}, nil
}

// TestRunCommandSpillsAndRetrievesOversizedOutput pins T8 end to end: an
// oversized command output is truncated with a marker, its full payload spilled
// to the workspace, and the exact continuation the marker points to is
// retrievable through read_file.
func TestRunCommandSpillsAndRetrievesOversizedOutput(t *testing.T) {
	ctx := context.Background()
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(ctx, "s", sandbox.Options{})
	big := strings.Repeat("x", spillCap+5000)
	bp := &bigExecPort{MemPort: sb, out: big}

	res, err := NewRunCommand(bp, h).Call(ctx, map[string]any{"command": "emit"})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Content, spillDir+"/") || !strings.Contains(res.Content, "read_file") {
		t.Errorf("no spill marker in result tail: %q", res.Content[spillKeepHead:])
	}
	if len(res.Content) >= len(big) {
		t.Errorf("result not shrunk below the full output: len=%d", len(res.Content))
	}

	// Locate the spilled file and page its tail back through read_file — the exact
	// continuation the marker instructs the model to fetch.
	var found string
	files, _ := sb.Walk(ctx, h, ".")
	for _, f := range files {
		if strings.HasPrefix(f, spillDir+"/") {
			found = f
		}
	}
	if found == "" {
		t.Fatal("run_command did not spill the full output")
	}
	if stored := readFile(t, sb, h, found); stored != big {
		t.Errorf("spill file len=%d, want the full %d bytes", len(stored), len(big))
	}
	rd, _ := (&fileReadTool{sb: sb, h: h}).Call(ctx, map[string]any{"path": found, "offset": spillKeepHead})
	if rd.Content != big[spillKeepHead:] {
		t.Errorf("read_file continuation len=%d, want %d", len(rd.Content), len(big)-spillKeepHead)
	}
}
