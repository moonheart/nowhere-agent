package builtin

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/toolruntime"
)

// SearchTools returns the content/path search tools bound to a session sandbox:
// grep (regex over file contents) and glob (path-pattern match). Both need the
// sandbox's Walker capability for a whole-tree view; if the backend does not
// implement Walker the tools report an is_error result rather than failing hard.
func SearchTools(sb sandbox.Port, h sandbox.Handle) []toolruntime.Tool {
	return []toolruntime.Tool{
		&grepTool{sb: sb, h: h},
		&globTool{sb: sb, h: h},
	}
}

const (
	grepMaxResults  = 200
	grepMaxFileSize = 1 << 20 // 1 MiB: skip files larger than this
	grepMaxLineLen  = 500     // truncate a matching line to this many bytes in output
)

// walker returns the Port's Walker capability, or an is_error Result explaining
// that the backend does not support tree search.
func walker(sb sandbox.Port) (sandbox.Walker, *toolruntime.Result) {
	w, ok := sb.(sandbox.Walker)
	if !ok {
		return nil, &toolruntime.Result{Content: "search is not supported by this sandbox backend", IsError: true}
	}
	return w, nil
}

type grepTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *grepTool) Name() string { return "grep" }
func (t *grepTool) Description() string {
	return "Search file contents by regular expression across the workspace, returning path:line:text matches. Optionally scope to a subtree (path) and filter files by a glob."
}
func (t *grepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":     map[string]any{"type": "string", "description": "regular expression (RE2 syntax) to match on each line"},
			"path":        map[string]any{"type": "string", "description": "workspace-relative subtree to search (default \".\")"},
			"glob":        map[string]any{"type": "string", "description": "only search files whose path matches this glob (e.g. \"**/*.go\")"},
			"max_results": map[string]any{"type": "integer", "description": "cap on matching lines returned (default 200)"},
		},
		"required": []string{"pattern"},
	}
}
func (t *grepTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (t *grepTool) Timeout() time.Duration { return 30 * time.Second }

func (t *grepTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	pattern, err := argString(args, "pattern")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("invalid pattern: %v", err), IsError: true}, nil
	}
	root := optString(args, "path", ".")
	max := grepMaxResults
	if v, ok := args["max_results"].(float64); ok && int(v) > 0 {
		max = int(v)
	}
	var fileGlob *regexp.Regexp
	if g := optString(args, "glob", ""); g != "" {
		fileGlob, err = globToRegexp(g)
		if err != nil {
			return toolruntime.Result{Content: fmt.Sprintf("invalid glob: %v", err), IsError: true}, nil
		}
	}

	w, errRes := walker(t.sb)
	if errRes != nil {
		return *errRes, nil
	}
	files, err := w.Walk(ctx, t.h, root)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("grep failed: %v", err), IsError: true}, nil
	}

	var lines []string
	truncated := false
	for _, f := range files {
		if fileGlob != nil && !fileGlob.MatchString(f) {
			continue
		}
		content, ok := t.readText(ctx, f)
		if !ok {
			continue // unreadable, binary, or too large
		}
		for i, line := range strings.Split(content, "\n") {
			line = strings.TrimRight(line, "\r")
			if !re.MatchString(line) {
				continue
			}
			if len(line) > grepMaxLineLen {
				line = line[:grepMaxLineLen] + "…"
			}
			lines = append(lines, fmt.Sprintf("%s:%d:%s", f, i+1, line))
			if len(lines) >= max {
				truncated = true
				break
			}
		}
		if truncated {
			break
		}
	}

	if len(lines) == 0 {
		return toolruntime.Result{Content: "(no matches)"}, nil
	}
	out := strings.Join(lines, "\n")
	if truncated {
		out += fmt.Sprintf("\n… (stopped at %d matches; narrow the pattern or path)", max)
	}
	return toolruntime.Result{Content: out}, nil
}

// readText reads a workspace file, returning (content, true) only if it is a
// readable, non-binary file within the size cap.
func (t *grepTool) readText(ctx context.Context, path string) (string, bool) {
	rc, err := t.sb.ReadFile(ctx, t.h, path)
	if err != nil {
		return "", false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, grepMaxFileSize+1))
	if err != nil || len(b) > grepMaxFileSize {
		return "", false
	}
	// Binary heuristic: a NUL byte in the head means "not text".
	head := b
	if len(head) > 8000 {
		head = head[:8000]
	}
	for _, c := range head {
		if c == 0 {
			return "", false
		}
	}
	return string(b), true
}

type globTool struct {
	sb sandbox.Port
	h  sandbox.Handle
}

func (t *globTool) Name() string { return "glob" }
func (t *globTool) Description() string {
	return "Find files whose workspace-relative path matches a glob pattern. Supports * (within a path segment) and ** (across segments), e.g. \"**/*.go\" or \"src/**/test_*.py\"."
}
func (t *globTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "glob pattern, e.g. \"**/*.go\""},
			"path":    map[string]any{"type": "string", "description": "workspace-relative subtree to search (default \".\")"},
		},
		"required": []string{"pattern"},
	}
}
func (t *globTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (t *globTool) Timeout() time.Duration { return 15 * time.Second }

func (t *globTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	pattern, err := argString(args, "pattern")
	if err != nil {
		return toolruntime.Result{Content: err.Error(), IsError: true}, nil
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("invalid glob: %v", err), IsError: true}, nil
	}
	root := optString(args, "path", ".")

	w, errRes := walker(t.sb)
	if errRes != nil {
		return *errRes, nil
	}
	files, err := w.Walk(ctx, t.h, root)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("glob failed: %v", err), IsError: true}, nil
	}
	var out []string
	for _, f := range files {
		if re.MatchString(f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return toolruntime.Result{Content: "(no matches)"}, nil
	}
	return toolruntime.Result{Content: strings.Join(out, "\n")}, nil
}

// optString reads an optional string argument, returning def when absent/empty.
func optString(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

// globToRegexp compiles a path glob into an anchored RE2 regexp. "*" matches
// within a single path segment (not "/"); "**" matches across segments; "**/"
// also matches zero segments so "**/x" matches a top-level "x"; "?" matches one
// non-separator char. All other regex metacharacters are escaped.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i += 2
				if i < len(glob) && glob[i] == '/' {
					b.WriteString("(?:.*/)?")
					i++
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString("[^/]*")
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
