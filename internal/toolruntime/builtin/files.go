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

// FileTools returns the built-in workspace tools bound to a session sandbox:
// read_file, write_file, list_dir, edit_file, the search tools (grep, glob),
// and the mutation tools (move_file, copy_file, delete_file, make_dir). Paths
// are workspace-relative; the sandbox backend rejects any escape.
func FileTools(sb sandbox.Port, h sandbox.Handle) []toolruntime.Tool {
	tools := []toolruntime.Tool{
		&fileReadTool{sb: sb, h: h},
		&fileWriteTool{sb: sb, h: h},
		&listDirTool{sb: sb, h: h},
		&editFileTool{sb: sb, h: h},
	}
	tools = append(tools, SearchTools(sb, h)...)
	return append(tools, FSOpsTools(sb, h)...)
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

// readFilePageBytes bounds a single read_file result. It is both the default
// and the maximum page size, and is kept equal to the persistence cap
// (contextmgmt.MaxToolResultChars) so a default page always persists whole — the
// tool never relies on the persistence-side truncation, and a big file is paged
// rather than slurped into the context window in one shot (capability-gap T8).
const readFilePageBytes = 20_000

type fileReadTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *fileReadTool) Name() string { return "read_file" }
func (t *fileReadTool) Description() string {
	return "Read a file from the session workspace. Reads up to a page (20000 bytes) at a time; when more remains the result ends with a marker telling you the offset to continue from. Pass offset to page through a large file."
}
func (t *fileReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "workspace-relative path to read"},
			"offset": map[string]any{"type": "integer", "description": "byte offset to start reading from (default 0)"},
			"limit":  map[string]any{"type": "integer", "description": "max bytes to return (default and max 20000)"},
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
	offset := optInt(args, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	limit := optInt(args, "limit", readFilePageBytes)
	if limit <= 0 || limit > readFilePageBytes {
		limit = readFilePageBytes
	}

	rc, err := t.sb.ReadFile(ctx, t.h, path)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("read_file failed: %v", err), IsError: true}, nil
	}
	defer rc.Close()
	// Read one byte past the requested window so we can tell whether more remains
	// without slurping (and OOM-ing on) a huge file. Byte-exact, so CRLF and any
	// other bytes round-trip untouched.
	data, err := io.ReadAll(io.LimitReader(rc, int64(offset)+int64(limit)+1))
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("read_file failed: %v", err), IsError: true}, nil
	}

	// offset at or past everything we read: the file is shorter than offset (we
	// only reach here when we did NOT hit the read cap, so len(data) is the true
	// size). Report it rather than returning an empty result.
	if offset >= len(data) {
		return toolruntime.Result{Content: fmt.Sprintf("(read_file: offset %d is at or past end of file; %d bytes total)", offset, len(data))}, nil
	}

	// Never split a multi-byte UTF-8 rune across a page boundary. Retreat the
	// start to the rune it lands in (a hand-picked offset may be mid-rune) and
	// cut the end on a rune boundary too, so every page is valid UTF-8 and the
	// continuation offset we hand back is itself rune-aligned — no stray U+FFFD
	// at page seams (capability-gap T8).
	start := runeBoundaryAtOrBefore(data, offset)
	end := start + limit
	more := len(data) > end // at least one byte beyond the window
	if !more {
		end = len(data)
	} else {
		end = runeBoundaryAtOrBefore(data, end)
		if end <= start {
			// A single rune is wider than the whole page budget: emit it whole so
			// we still make progress and never split it. Only reachable with a
			// pathologically tiny limit; a default page dwarfs any rune.
			end = start + 1
			for end < len(data) && data[end]&0xC0 == 0x80 {
				end++
			}
		}
	}
	page := string(data[start:end])
	if more {
		page += fmt.Sprintf("\n\n[read_file: returned bytes %d-%d; more remain, call read_file with offset=%d to continue]", start, end, end)
	}
	return toolruntime.Result{Content: page}, nil
}

// optInt reads an optional integer argument, returning def when absent or of an
// unexpected type. JSON numbers decode to float64; a Go-constructed int/int64
// (tests, direct callers) is accepted too.
func optInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return def
}

// runeBoundaryAtOrBefore returns the largest index j in [0, i] at which s does
// not sit inside a multi-byte UTF-8 rune — i.e. j == 0, j == len(s), or s[j] is
// not a UTF-8 continuation byte (10xxxxxx). Slicing s at j therefore never
// splits a rune, keeping paged reads and spill heads valid UTF-8 across
// boundaries (capability-gap T8). It works on either a string or a byte slice.
func runeBoundaryAtOrBefore[T string | []byte](s T, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && s[i]&0xC0 == 0x80 {
		i--
	}
	return i
}

type fileWriteTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *fileWriteTool) Name() string { return "write_file" }
func (t *fileWriteTool) Description() string {
	return "Write content to a file in the session workspace (overwrites)"
}
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

func (t *listDirTool) Name() string { return "list_dir" }
func (t *listDirTool) Description() string {
	return "List entries in a directory of the session workspace"
}
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
	if len(names) == 0 {
		return toolruntime.Result{Content: "(empty directory)"}, nil
	}
	return toolruntime.Result{Content: strings.Join(names, "\n")}, nil
}
