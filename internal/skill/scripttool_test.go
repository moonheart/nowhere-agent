package skill

import (
	"context"
	"io"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/sandbox"
)

// fakeSandbox captures what the skill-script runner stages and runs so a test
// can assert the C17 contract: the script lands in a confined .skills path and
// the model's args arrive as separate argv entries, never concatenated into a
// shell line.
type fakeSandbox struct {
	writes  map[string]string // path -> staged content
	execArg []string          // last argv passed to Exec
	execRes sandbox.ExecResult
	execErr error
}

func newFakeSandbox() *fakeSandbox { return &fakeSandbox{writes: map[string]string{}} }

func (f *fakeSandbox) Create(context.Context, string, sandbox.Options) (sandbox.Handle, error) {
	return sandbox.Handle{}, nil
}
func (f *fakeSandbox) Destroy(context.Context, sandbox.Handle) error { return nil }

func (f *fakeSandbox) Exec(_ context.Context, _ sandbox.Handle, cmd []string) (sandbox.ExecResult, error) {
	f.execArg = append([]string(nil), cmd...)
	return f.execRes, f.execErr
}

func (f *fakeSandbox) ReadFile(context.Context, sandbox.Handle, string) (io.ReadCloser, error) {
	return nil, nil
}

func (f *fakeSandbox) WriteFile(_ context.Context, _ sandbox.Handle, path string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.writes[path] = string(b)
	return nil
}

func (f *fakeSandbox) ListDir(context.Context, sandbox.Handle, string) ([]string, error) {
	return nil, nil
}
func (f *fakeSandbox) Move(context.Context, sandbox.Handle, string, string) error { return nil }
func (f *fakeSandbox) Copy(context.Context, sandbox.Handle, string, string) error { return nil }
func (f *fakeSandbox) Delete(context.Context, sandbox.Handle, string) error       { return nil }
func (f *fakeSandbox) Mkdir(context.Context, sandbox.Handle, string) error        { return nil }

// scriptStore seeds one skill with scripts and returns an Engine over it, for
// driving the fixed run_skill_script tool.
func scriptEngine(t *testing.T, sk Skill) *Engine {
	t.Helper()
	st := newMemStore()
	if sk.Scope.Scope == "" {
		sk.Scope = identity.SystemScope()
	}
	if _, err := st.Put(context.Background(), sk, "test"); err != nil {
		t.Fatal(err)
	}
	return NewEngine(st)
}

var sysScopes = []identity.ScopeRef{identity.SystemScope()}

// TestRunSkillScriptToolFixed: the runner exposes ONE constant tool name and a
// sandbox-write risk, regardless of how many scripts exist — the property that
// keeps the LLM's tool-prefix cache stable.
func TestRunSkillScriptToolFixed(t *testing.T) {
	e := scriptEngine(t, Skill{Name: "calc", Scripts: map[string]string{"a.py": "1", "b.py": "2", "c.sh": "3"}})
	tool := NewRunSkillScript(e, sysScopes, newFakeSandbox(), sandbox.Handle{})
	if tool.Name() != RunSkillScriptName {
		t.Errorf("name = %q, want the fixed %q", tool.Name(), RunSkillScriptName)
	}
	if tool.Risk() != "sandbox_write" {
		t.Errorf("risk = %q", tool.Risk())
	}
	s := tool.Schema()
	req, _ := s["required"].([]string)
	if len(req) != 2 {
		t.Errorf("schema required = %v, want [skill script]", req)
	}
}

// TestRunSkillScriptStagesConfinedAndSelectsInterpreter: the script body is
// written to .skills/<skill>/<script> and executed as [interpreter, path].
func TestRunSkillScriptStagesConfinedAndSelectsInterpreter(t *testing.T) {
	f := newFakeSandbox()
	e := scriptEngine(t, Skill{Name: "data tools", Scripts: map[string]string{"report.py": "print('hi')"}})
	tool := NewRunSkillScript(e, sysScopes, f, sandbox.Handle{ID: "h"})

	if _, err := tool.Call(context.Background(), map[string]any{"skill": "data tools", "script": "report.py"}); err != nil {
		t.Fatal(err)
	}

	// sanitize maps the "." in "report.py" to "_", then extOf re-appends ".py".
	wantPath := ".skills/data_tools/report_py.py"
	body, ok := f.writes[wantPath]
	if !ok {
		t.Fatalf("no write to %q; writes = %v", wantPath, keys(f.writes))
	}
	if body != "print('hi')" {
		t.Errorf("staged body = %q", body)
	}
	if len(f.execArg) != 2 || f.execArg[0] != "python3" || f.execArg[1] != wantPath {
		t.Errorf("argv = %v, want [python3 %s]", f.execArg, wantPath)
	}
}

// TestRunSkillScriptArgsAreArgvNotShell: the C17 core — a metacharacter-laden
// args string must reach the interpreter as inert argv entries, NOT be
// concatenated into a `sh -c` command where it would inject.
func TestRunSkillScriptArgsAreArgvNotShell(t *testing.T) {
	f := newFakeSandbox()
	e := scriptEngine(t, Skill{Name: "s", Scripts: map[string]string{"run.py": "pass"}})
	tool := NewRunSkillScript(e, sysScopes, f, sandbox.Handle{})

	injection := `; rm -rf / && echo pwned`
	if _, err := tool.Call(context.Background(), map[string]any{"skill": "s", "script": "run.py", "args": injection}); err != nil {
		t.Fatal(err)
	}

	if f.execArg[0] != "python3" {
		t.Fatalf("argv[0] = %q, want python3 (not a shell)", f.execArg[0])
	}
	for _, a := range f.execArg {
		if a == "sh" || a == "-c" {
			t.Fatalf("argv contains a shell invocation %q: args were concatenated into a shell line", a)
		}
	}
	joined := strings.Join(f.execArg[2:], " ")
	if !strings.Contains(joined, ";") || !strings.Contains(joined, "rm") {
		t.Errorf("injection tokens not passed through as argv: %v", f.execArg[2:])
	}
}

// TestRunSkillScriptUnknownSkillOrScript: a typo'd skill or script is a
// self-correctable error result that names what went wrong, not a crash.
func TestRunSkillScriptUnknownSkillOrScript(t *testing.T) {
	f := newFakeSandbox()
	e := scriptEngine(t, Skill{Name: "calc", Scripts: map[string]string{"run.py": "1"}})
	tool := NewRunSkillScript(e, sysScopes, f, sandbox.Handle{})

	noSkill, _ := tool.Call(context.Background(), map[string]any{"script": "run.py"})
	if !noSkill.IsError {
		t.Errorf("missing skill arg should be an error result: %+v", noSkill)
	}
	noScript, _ := tool.Call(context.Background(), map[string]any{"skill": "calc"})
	if !noScript.IsError {
		t.Errorf("missing script arg should be an error result: %+v", noScript)
	}
	unknown, _ := tool.Call(context.Background(), map[string]any{"skill": "calc", "script": "nope.py"})
	if !unknown.IsError || !strings.Contains(unknown.Content, "no script") {
		t.Errorf("unknown script should be an error result, got %+v", unknown)
	}
	if len(f.execArg) != 0 {
		t.Errorf("nothing should execute for an unknown script, got %v", f.execArg)
	}
}

// TestRunSkillScriptRespectsScope: a skill outside the caller's scopes cannot be
// run (scope is enforced at resolution time).
func TestRunSkillScriptRespectsScope(t *testing.T) {
	f := newFakeSandbox()
	e := scriptEngine(t, Skill{Name: "secret", Scope: identity.UserScope("user1"), Scripts: map[string]string{"run.sh": "echo x"}})
	// A different user resolves only their own + system scope.
	tool := NewRunSkillScript(e, []identity.ScopeRef{identity.UserScope("user2"), identity.SystemScope()}, f, sandbox.Handle{})
	res, _ := tool.Call(context.Background(), map[string]any{"skill": "secret", "script": "run.sh"})
	if !res.IsError {
		t.Errorf("a script in another user's scope must not run, got %+v", res)
	}
	if len(f.execArg) != 0 {
		t.Errorf("nothing should execute across scopes, got %v", f.execArg)
	}
}

// TestRunSkillScriptExecResult: stdout/stderr are surfaced and a non-zero exit
// marks the result an error.
func TestRunSkillScriptExecResult(t *testing.T) {
	f := newFakeSandbox()
	f.execRes = sandbox.ExecResult{ExitCode: 1, Stdout: "partial", Stderr: "boom"}
	e := scriptEngine(t, Skill{Name: "s", Scripts: map[string]string{"run.sh": "exit 1"}})
	tool := NewRunSkillScript(e, sysScopes, f, sandbox.Handle{})

	res, err := tool.Call(context.Background(), map[string]any{"skill": "s", "script": "run.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("exit code 1 should mark IsError")
	}
	if !strings.Contains(res.Content, "partial") || !strings.Contains(res.Content, "boom") {
		t.Errorf("content = %q, want stdout and stderr", res.Content)
	}
}

// resolverSandbox is a fakeSandbox that also implements InterpreterResolver, so
// a test can drive which interpreter the backend reports as usable.
type resolverSandbox struct {
	*fakeSandbox
	pick string // what ResolveInterpreter returns; "" means none available
	got  []string
}

func (r *resolverSandbox) ResolveInterpreter(_ context.Context, _ sandbox.Handle, candidates []string) string {
	r.got = append([]string(nil), candidates...)
	if r.pick == "" {
		return ""
	}
	return r.pick
}

// TestRunSkillScriptUsesBackendInterpreter: when the backend resolves an
// interpreter (e.g. py ahead of a Windows Store python3 stub), that choice is
// what runs.
func TestRunSkillScriptUsesBackendInterpreter(t *testing.T) {
	f := &resolverSandbox{fakeSandbox: newFakeSandbox(), pick: "py"}
	e := scriptEngine(t, Skill{Name: "s", Scripts: map[string]string{"run.py": "print(1+1)"}})
	tool := NewRunSkillScript(e, sysScopes, f, sandbox.Handle{})

	if _, err := tool.Call(context.Background(), map[string]any{"skill": "s", "script": "run.py"}); err != nil {
		t.Fatal(err)
	}
	if f.execArg[0] != "py" {
		t.Errorf("argv[0] = %q, want the backend-resolved py", f.execArg[0])
	}
	if len(f.got) != 3 || f.got[0] != "python3" {
		t.Errorf("resolver candidates = %v, want [python3 python py]", f.got)
	}
}

// TestRunSkillScriptNoInterpreterAvailable: when the backend finds no usable
// interpreter, the tool returns a clear, actionable error rather than a bare
// nonzero exit with empty output.
func TestRunSkillScriptNoInterpreterAvailable(t *testing.T) {
	f := &resolverSandbox{fakeSandbox: newFakeSandbox(), pick: ""}
	e := scriptEngine(t, Skill{Name: "s", Scripts: map[string]string{"run.py": "print(1)"}})
	tool := NewRunSkillScript(e, sysScopes, f, sandbox.Handle{})

	res, err := tool.Call(context.Background(), map[string]any{"skill": "s", "script": "run.py"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("no interpreter should mark IsError")
	}
	if !strings.Contains(res.Content, "no interpreter") || !strings.Contains(res.Content, "python3") {
		t.Errorf("content = %q, want an actionable missing-interpreter message", res.Content)
	}
	if len(f.execArg) != 0 {
		t.Errorf("exec should not run without an interpreter, got %v", f.execArg)
	}
}

// TestInterpreterCandidates pins the extension → interpreter whitelist. The
// first candidate is the conventional default; the rest are fallbacks a backend
// may prefer (e.g. py/python ahead of a Windows Store python3 stub).
func TestInterpreterCandidates(t *testing.T) {
	cases := map[string][]string{
		"run.py":     {"python3", "python", "py"},
		"build.js":   {"node"},
		"mod.mjs":    {"node"},
		"setup.sh":   {"sh", "bash"},
		"setup.bash": {"sh", "bash"},
		"noext":      {"sh", "bash"},              // extensionless defaults to a POSIX shell
		"weird.rb":   {"sh", "bash"},              // unknown extensions fall back to sh, never a guessed runtime
		"UPPER.PY":   {"python3", "python", "py"}, // case-insensitive
	}
	for name, want := range cases {
		got := interpreterCandidates(name)
		if len(got) != len(want) {
			t.Fatalf("interpreterCandidates(%q) = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("interpreterCandidates(%q)[%d] = %q, want %q", name, i, got[i], want[i])
			}
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
