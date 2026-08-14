package skill

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/toolruntime"
)

// RunSkillScriptTool is the single, fixed tool that runs a skill's L2 script in
// the session sandbox (design D7). It replaces the earlier one-tool-per-script
// registration: every script used to become its own `skill_<skill>_<script>`
// tool, which (a) inflated the tools array linearly with the number of scripts
// and (b) changed that array on every skill edit — and the tools array sits in
// the LLM's cacheable prompt prefix, so any change shattered the prefix cache.
// One constant tool keeps the prefix byte-stable regardless of how many scripts
// exist; the script is chosen by name via the call arguments, not the tool set.
//
// Execution stays C17-safe: the resolved script body is written to a
// workspace-confined file and run under an interpreter chosen by file extension,
// with the model's "args" passed as separate argv entries — never concatenated
// into a shell command line. Scripts are resolved lazily against the caller's
// scopes at call time, so the tool reflects the current skill store and honours
// user>team>system priority.
type RunSkillScriptTool struct {
	engine  *Engine
	scopes  []identity.ScopeRef
	sandbox sandbox.Port
	handle  sandbox.Handle
}

// RunSkillScriptName is the fixed tool name.
const RunSkillScriptName = "run_skill_script"

// NewRunSkillScript creates the fixed skill-script runner bound to a sandbox.
func NewRunSkillScript(engine *Engine, scopes []identity.ScopeRef, sb sandbox.Port, h sandbox.Handle) *RunSkillScriptTool {
	return &RunSkillScriptTool{engine: engine, scopes: scopes, sandbox: sb, handle: h}
}

func (t *RunSkillScriptTool) Name() string { return RunSkillScriptName }

func (t *RunSkillScriptTool) Description() string {
	return "Run one of a skill's scripts in the session workspace and return its stdout, " +
		"stderr, and exit code. The Available skills index lists which scripts each skill " +
		"has (the \"scripts:\" line); call load_skill on a skill for details. Pass the skill " +
		"name and one of its script names. A non-zero exit code is reported as output, not a tool error."
}

// Schema takes the skill name, the script name within it, and an optional "args"
// string split on whitespace into individual argv entries.
func (t *RunSkillScriptTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill":  map[string]any{"type": "string", "description": "skill name from the Available skills index"},
			"script": map[string]any{"type": "string", "description": "one of the skill's script names (the \"scripts:\" line / load_skill output)"},
			"args":   map[string]any{"type": "string", "description": "optional whitespace-separated arguments passed to the script as individual argv entries"},
		},
		"required": []string{"skill", "script"},
	}
}

// Risk is sandbox-write: scripts run inside the session sandbox.
func (t *RunSkillScriptTool) Risk() toolruntime.Risk { return toolruntime.RiskSandboxWrite }

// Timeout bounds script execution.
func (t *RunSkillScriptTool) Timeout() time.Duration { return 60 * time.Second }

// interpreterCandidates lists the interpreters to try for a script, in priority
// order, by file extension. The whitelist is the C17 boundary: a script runs
// only under a known interpreter, never an arbitrary shell string. Which
// candidate actually runs is decided per-backend (sandbox.InterpreterResolver) —
// on a Windows host the Store `python3` shim is skipped in favour of `py` /
// `python`, while a Linux container keeps the conventional python3-first order.
func interpreterCandidates(scriptPath string) []string {
	switch strings.ToLower(path.Ext(scriptPath)) {
	case ".py":
		return []string{"python3", "python", "py"}
	case ".js", ".mjs":
		return []string{"node"}
	case ".sh", ".bash", "":
		return []string{"sh", "bash"}
	default:
		return []string{"sh", "bash"}
	}
}

// resolveInterpreter picks the interpreter argv for a script in this sandbox. It
// asks the backend which candidate is usable (sidestepping host shims like the
// Windows Store python3 stub); a backend without that capability uses the first
// candidate. Returns "" when no candidate is available.
func (t *RunSkillScriptTool) resolveInterpreter(ctx context.Context, scriptName string) string {
	candidates := interpreterCandidates(scriptName)
	if r, ok := t.sandbox.(sandbox.InterpreterResolver); ok {
		return r.ResolveInterpreter(ctx, t.handle, candidates)
	}
	return candidates[0]
}

// scriptDir is the workspace-relative directory skill scripts are staged into.
const scriptDir = ".skills"

// Call resolves the named script against the caller's scopes, stages it into the
// sandbox, and runs it under its interpreter. The model's "args" become trailing
// argv entries (whitespace-split), NOT part of a shell command line, so
// metacharacters cannot inject commands.
func (t *RunSkillScriptTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	skillName, _ := args["skill"].(string)
	if strings.TrimSpace(skillName) == "" {
		return toolruntime.Result{Content: `missing required argument "skill"`, IsError: true}, nil
	}
	scriptName, _ := args["script"].(string)
	if strings.TrimSpace(scriptName) == "" {
		return toolruntime.Result{Content: `missing required argument "script"`, IsError: true}, nil
	}

	script, ok, err := t.engine.LoadL2Script(ctx, skillName, scriptName, t.scopes)
	if err != nil {
		return toolruntime.Result{}, err
	}
	if !ok {
		return toolruntime.Result{Content: fmt.Sprintf(
			"skill %q has no script %q (or the skill is unknown) — check the \"scripts:\" line in the Available skills index or call load_skill",
			skillName, scriptName), IsError: true}, nil
	}

	// Stage the script at a workspace-confined path derived from the skill and
	// script names. sanitize strips path separators, so this cannot escape the
	// sandbox (WriteFile additionally confines via resolve()).
	relPath := path.Join(scriptDir, sanitize(skillName), sanitize(scriptName)+extOf(scriptName))
	if err := t.sandbox.WriteFile(ctx, t.handle, relPath, strings.NewReader(script)); err != nil {
		return toolruntime.Result{}, fmt.Errorf("stage script: %w", err)
	}

	argv := []string{}
	interp := t.resolveInterpreter(ctx, scriptName)
	if interp == "" {
		// A clear, actionable error beats a bare nonzero exit with no output: the
		// model can report the missing runtime instead of guessing.
		return toolruntime.Result{
			Content: fmt.Sprintf("no interpreter for %s is available in this sandbox (tried %s)",
				scriptName, strings.Join(interpreterCandidates(scriptName), ", ")),
			IsError: true,
		}, nil
	}
	argv = append(argv, interp, relPath)
	if a, ok := args["args"].(string); ok && a != "" {
		argv = append(argv, strings.Fields(a)...)
	}

	res, err := t.sandbox.Exec(ctx, t.handle, argv)
	if err != nil {
		return toolruntime.Result{}, fmt.Errorf("sandbox exec: %w", err)
	}
	out := res.Stdout
	if res.Stderr != "" {
		out += "\n[stderr]\n" + res.Stderr
	}
	return toolruntime.Result{
		Content: strings.TrimSpace(out),
		IsError: res.ExitCode != 0,
	}, nil
}

// extOf returns the script's original extension (for the staged filename), or
// ".sh" when it has none, so the staged file name matches its interpreter.
func extOf(scriptName string) string {
	if ext := path.Ext(scriptName); ext != "" {
		return ext
	}
	return ".sh"
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
