package skill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/toolruntime"
)

// ScriptTool exposes a skill's L2 script as a Tool that executes in the
// session sandbox (design D7: scripts execute in the sandbox via SandboxPort).
type ScriptTool struct {
	skillName  string
	scriptName string
	script     string
	desc       string
	sandbox    sandbox.Port
	handle     sandbox.Handle
}

// NewScriptTool wraps a skill script as a sandbox-executing tool.
func NewScriptTool(skillName, scriptName, script, desc string, sb sandbox.Port, h sandbox.Handle) *ScriptTool {
	return &ScriptTool{
		skillName:  skillName,
		scriptName: scriptName,
		script:     script,
		desc:       desc,
		sandbox:    sb,
		handle:     h,
	}
}

// Name is namespaced to avoid collisions: "skill_<skill>_<script>".
func (t *ScriptTool) Name() string {
	return fmt.Sprintf("skill_%s_%s", sanitize(t.skillName), sanitize(t.scriptName))
}

// Description returns the tool description.
func (t *ScriptTool) Description() string { return t.desc }

// Schema accepts an optional "args" string passed to the script.
func (t *ScriptTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{"type": "string", "description": "arguments passed to the script"},
		},
	}
}

// Risk is sandbox-write: scripts run inside the session sandbox.
func (t *ScriptTool) Risk() toolruntime.Risk { return toolruntime.RiskSandboxWrite }

// Timeout bounds script execution.
func (t *ScriptTool) Timeout() time.Duration { return 60 * time.Second }

// Call runs the script in the sandbox via `sh -c`, returning combined output.
func (t *ScriptTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	cmd := t.script
	if a, ok := args["args"].(string); ok && a != "" {
		cmd = cmd + " " + a
	}
	res, err := t.sandbox.Exec(ctx, t.handle, []string{"sh", "-c", cmd})
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
