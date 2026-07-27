package builtin

import (
	"context"
	"fmt"
	"time"

	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/toolruntime"
)

// FSOpsTools returns the filesystem-mutation tools bound to a session sandbox:
// move_file, copy_file, delete_file, make_dir (capability-gap T7). Together
// with read/write/list/edit these complete the workspace verb set so the agent
// can restructure and clean up files, not just read and overwrite them. Each
// tool delegates to a Port verb, so the sandbox backend (not the tool) enforces
// confinement — on the local backend both source and destination are resolved
// against the workspace and any escape is rejected.
func FSOpsTools(sb sandbox.Port, h sandbox.Handle) []toolruntime.Tool {
	return []toolruntime.Tool{
		&moveFileTool{sb: sb, h: h},
		&copyFileTool{sb: sb, h: h},
		&deleteFileTool{sb: sb, h: h},
		&makeDirTool{sb: sb, h: h},
	}
}

// srcDstSchema is the shared input schema for the two-path tools (move/copy).
func srcDstSchema(verb string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"src": map[string]any{"type": "string", "description": "workspace-relative source path to " + verb},
			"dst": map[string]any{"type": "string", "description": "workspace-relative destination path"},
		},
		"required": []string{"src", "dst"},
	}
}

type moveFileTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *moveFileTool) Name() string { return "move_file" }
func (t *moveFileTool) Description() string {
	return "Move or rename a file or directory in the session workspace. Works on both files and directories; the destination's parent is created as needed."
}
func (t *moveFileTool) Schema() map[string]any     { return srcDstSchema("move") }
func (t *moveFileTool) Risk() toolruntime.Risk     { return toolruntime.RiskSandboxWrite }
func (t *moveFileTool) Timeout() time.Duration     { return 15 * time.Second }

func (t *moveFileTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	src, err := argString(args, "src")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	dst, err := argString(args, "dst")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	if err := t.sb.Move(ctx, t.h, src, dst); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("move_file failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: fmt.Sprintf("moved %s to %s", src, dst)}, nil
}

type copyFileTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *copyFileTool) Name() string { return "copy_file" }
func (t *copyFileTool) Description() string {
	return "Copy a file or directory in the session workspace. Directories are copied recursively; the destination's parent is created as needed."
}
func (t *copyFileTool) Schema() map[string]any     { return srcDstSchema("copy") }
func (t *copyFileTool) Risk() toolruntime.Risk     { return toolruntime.RiskSandboxWrite }
func (t *copyFileTool) Timeout() time.Duration     { return 30 * time.Second }

func (t *copyFileTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	src, err := argString(args, "src")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	dst, err := argString(args, "dst")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	if err := t.sb.Copy(ctx, t.h, src, dst); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("copy_file failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: fmt.Sprintf("copied %s to %s", src, dst)}, nil
}

type deleteFileTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *deleteFileTool) Name() string { return "delete_file" }
func (t *deleteFileTool) Description() string {
	return "Delete a file or directory from the session workspace. Deleting a directory removes everything under it and requires recursive=true; deleting a file needs no flag."
}
func (t *deleteFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":      map[string]any{"type": "string", "description": "workspace-relative path to delete"},
			"recursive": map[string]any{"type": "boolean", "description": "required (true) to delete a directory and its contents; ignored for files"},
		},
		"required": []string{"path"},
	}
}
func (t *deleteFileTool) Risk() toolruntime.Risk { return toolruntime.RiskSandboxWrite }
func (t *deleteFileTool) Timeout() time.Duration { return 30 * time.Second }

func (t *deleteFileTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	path, err := argString(args, "path")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	// Guard against wiping a directory by accident: deleting a directory requires
	// an explicit recursive=true. We detect "is a file" by trying to read it —
	// ReadFile succeeds on a file and fails on a directory (os.Open+read / tar /
	// mem key lookup all do), which is the one signal every backend agrees on.
	// A missing path also fails ReadFile and passes through; the backend reports
	// not-found. This deliberately does NOT use ListDir, whose "does this path
	// name a directory" semantics differ across backends (mem lists everything).
	isFile := false
	if rc, readErr := t.sb.ReadFile(ctx, t.h, path); readErr == nil {
		rc.Close()
		isFile = true
	}
	if !isFile {
		if recursive, _ := args["recursive"].(bool); !recursive {
			return toolruntime.Result{Content: fmt.Sprintf("%s is a directory (or does not exist); to delete a directory and its contents, set recursive=true", path), IsError: true}, nil
		}
	}
	if err := t.sb.Delete(ctx, t.h, path); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("delete_file failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: fmt.Sprintf("deleted %s", path)}, nil
}

type makeDirTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *makeDirTool) Name() string { return "make_dir" }
func (t *makeDirTool) Description() string {
	return "Create a directory (and any missing parents) in the session workspace."
}
func (t *makeDirTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "workspace-relative directory to create"},
		},
		"required": []string{"path"},
	}
}
func (t *makeDirTool) Risk() toolruntime.Risk { return toolruntime.RiskSandboxWrite }
func (t *makeDirTool) Timeout() time.Duration { return 15 * time.Second }

func (t *makeDirTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	path, err := argString(args, "path")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	if err := t.sb.Mkdir(ctx, t.h, path); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("make_dir failed: %v", err), IsError: true}, nil
	}
	return toolruntime.Result{Content: fmt.Sprintf("created directory %s", path)}, nil
}
