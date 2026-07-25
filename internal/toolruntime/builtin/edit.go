package builtin

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/toolruntime"
)

// editFileTool performs a precise, byte-exact string replacement inside a
// workspace file — the standard "str-replace" edit primitive. Unlike write_file
// (whole-file overwrite), it lets the model change a fragment of a large file
// without re-emitting the entire content (which is token-heavy and risks
// clobbering). Matching is byte-exact so nothing outside the replaced span is
// altered, which is what preserves line endings (CRLF) and surrounding
// whitespace.
type editFileTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *editFileTool) Name() string { return "edit_file" }
func (t *editFileTool) Description() string {
	return "Replace an exact substring in a workspace file. old_string must match exactly once (include surrounding context to disambiguate) unless replace_all is set; new_string may be empty to delete. Line endings and surrounding text are preserved."
}
func (t *editFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":        map[string]any{"type": "string", "description": "workspace-relative path to edit"},
			"old_string":  map[string]any{"type": "string", "description": "exact text to replace; must be unique unless replace_all"},
			"new_string":  map[string]any{"type": "string", "description": "replacement text (may be empty to delete old_string)"},
			"replace_all": map[string]any{"type": "boolean", "description": "replace every occurrence instead of requiring a unique match (default false)"},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}
func (t *editFileTool) Risk() toolruntime.Risk { return toolruntime.RiskSandboxWrite }
func (t *editFileTool) Timeout() time.Duration { return 15 * time.Second }

func (t *editFileTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	path, err := argString(args, "path")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	oldStr, err := argString(args, "old_string")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	newStr, err := argString(args, "new_string")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	if oldStr == "" {
		return toolruntime.Result{Content: "old_string must not be empty", IsError: true}, nil
	}
	replaceAll, _ := args["replace_all"].(bool)

	rc, err := t.sb.ReadFile(ctx, t.h, path)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("edit_file failed: %v", err), IsError: true}, nil
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("edit_file failed: %v", err), IsError: true}, nil
	}
	content := string(b)

	// Match byte-exactly first. Fallback: if the model supplied an LF old_string
	// but the file uses CRLF, retry with old/new translated to CRLF — this lets an
	// LF-emitting model edit a CRLF file while keeping the file's CRLF endings,
	// without us ever normalizing lines we don't touch.
	find, repl := oldStr, newStr
	n := strings.Count(content, find)
	if n == 0 && strings.Contains(content, "\r\n") && strings.Contains(oldStr, "\n") && !strings.Contains(oldStr, "\r\n") {
		find = strings.ReplaceAll(oldStr, "\n", "\r\n")
		repl = strings.ReplaceAll(newStr, "\n", "\r\n")
		n = strings.Count(content, find)
	}
	if n == 0 {
		return toolruntime.Result{Content: fmt.Sprintf("old_string not found in %s", path), IsError: true}, nil
	}
	if n > 1 && !replaceAll {
		return toolruntime.Result{Content: fmt.Sprintf("old_string appears %d times in %s; add surrounding context to make it unique, or set replace_all=true", n, path), IsError: true}, nil
	}

	updated := content
	if replaceAll {
		updated = strings.ReplaceAll(content, find, repl)
	} else {
		updated = strings.Replace(content, find, repl, 1)
	}
	if err := t.sb.WriteFile(ctx, t.h, path, strings.NewReader(updated)); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("edit_file failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: fmt.Sprintf("edited %s (%d replacement(s))", path, n)}, nil
}
