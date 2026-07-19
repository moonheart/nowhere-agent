// Package builtin holds the built-in tools exposed to the agent loop. Each
// tool is constructed per session and bound to that session's sandbox
// (design file-tools D1), so a tool physically cannot address another session's
// files — confinement is enforced by the sandbox backend, not the tool.
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

// FileTools returns the built-in file tools bound to a session sandbox:
// read_file, write_file, and list_dir. Paths are workspace-relative; the
// sandbox backend rejects any escape.
func FileTools(sb sandbox.Port, h sandbox.Handle) []toolruntime.Tool {
	return []toolruntime.Tool{
		&fileReadTool{sb: sb, h: h},
		&fileWriteTool{sb: sb, h: h},
		&listDirTool{sb: sb, h: h},
	}
}

// argString extracts a required string argument, reporting the missing key.
func argString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

type fileReadTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *fileReadTool) Name() string        { return "read_file" }
func (t *fileReadTool) Description() string { return "Read a file from the session workspace" }
func (t *fileReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "workspace-relative path to read"},
		},
		"required": []string{"path"},
	}
}
func (t *fileReadTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (t *fileReadTool) Timeout() time.Duration { return 15 * time.Second }

func (t *fileReadTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	path, err := argString(args, "path")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	rc, err := t.sb.ReadFile(ctx, t.h, path)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("read_file failed: %v", err), IsError: true}, nil
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("read_file failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: string(b)}, nil
}

type fileWriteTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *fileWriteTool) Name() string        { return "write_file" }
func (t *fileWriteTool) Description() string { return "Write content to a file in the session workspace (overwrites)" }
func (t *fileWriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "workspace-relative path to write"},
			"content": map[string]any{"type": "string", "description": "full content to write"},
		},
		"required": []string{"path", "content"},
	}
}
func (t *fileWriteTool) Risk() toolruntime.Risk { return toolruntime.RiskSandboxWrite }
func (t *fileWriteTool) Timeout() time.Duration { return 15 * time.Second }

func (t *fileWriteTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	path, err := argString(args, "path")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	content, err := argString(args, "content")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	if err := t.sb.WriteFile(ctx, t.h, path, strings.NewReader(content)); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("write_file failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(content), path)}, nil
}

type listDirTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *listDirTool) Name() string        { return "list_dir" }
func (t *listDirTool) Description() string { return "List entries in a directory of the session workspace" }
func (t *listDirTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "workspace-relative directory to list (default \".\")"},
		},
	}
}
func (t *listDirTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (t *listDirTool) Timeout() time.Duration { return 15 * time.Second }

func (t *listDirTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	path := "."
	if v, ok := args["path"]; ok {
		if s, ok := v.(string); ok && s != "" {
			path = s
		}
	}
	names, err := t.sb.ListDir(ctx, t.h, path)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("list_dir failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: strings.Join(names, "\n")}, nil
}
