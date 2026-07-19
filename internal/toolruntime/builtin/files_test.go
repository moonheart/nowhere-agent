package builtin

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/toolruntime"
)

func newMemTools(t *testing.T) (sandbox.Port, sandbox.Handle, []toolruntime.Tool) {
	t.Helper()
	sb := sandbox.NewMemPort()
	h, err := sb.Create(context.Background(), "sess1", sandbox.Options{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return sb, h, FileTools(sb, h)
}

func toolByName(tools []toolruntime.Tool, name string) toolruntime.Tool {
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	return nil
}

func TestFileToolsProvidesThree(t *testing.T) {
	_, _, tools := newMemTools(t)
	for _, want := range []string{"read_file", "write_file", "list_dir"} {
		if toolByName(tools, want) == nil {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestFileToolRiskClassification(t *testing.T) {
	_, _, tools := newMemTools(t)
	if r := toolByName(tools, "read_file").Risk(); r != toolruntime.RiskReadOnly {
		t.Errorf("read_file risk = %q", r)
	}
	if r := toolByName(tools, "list_dir").Risk(); r != toolruntime.RiskReadOnly {
		t.Errorf("list_dir risk = %q", r)
	}
	if r := toolByName(tools, "write_file").Risk(); r != toolruntime.RiskSandboxWrite {
		t.Errorf("write_file risk = %q", r)
	}
}

func TestWriteThenReadThroughTools(t *testing.T) {
	_, _, tools := newMemTools(t)
	ctx := context.Background()

	wr, _ := toolByName(tools, "write_file").Call(ctx, map[string]any{
		"path": "notes/a.txt", "content": "hello agent",
	})
	if wr.IsError {
		t.Fatalf("write failed: %s", wr.Content)
	}

	rd, _ := toolByName(tools, "read_file").Call(ctx, map[string]any{"path": "notes/a.txt"})
	if rd.IsError {
		t.Fatalf("read failed: %s", rd.Content)
	}
	if rd.Content != "hello agent" {
		t.Errorf("read content = %q", rd.Content)
	}
}

func TestReadMissingFileIsErrorResult(t *testing.T) {
	_, _, tools := newMemTools(t)
	res, _ := toolByName(tools, "read_file").Call(context.Background(), map[string]any{"path": "nope.txt"})
	if !res.IsError {
		t.Error("expected IsError for missing file")
	}
}

func TestMissingArgIsErrorResult(t *testing.T) {
	_, _, tools := newMemTools(t)
	res, _ := toolByName(tools, "write_file").Call(context.Background(), map[string]any{"path": "a.txt"})
	if !res.IsError || !strings.Contains(res.Content, "content") {
		t.Errorf("expected missing-content error, got %+v", res)
	}
}

func TestListDirTool(t *testing.T) {
	sb, h, tools := newMemTools(t)
	_ = sb.WriteFile(context.Background(), h, "x.txt", strings.NewReader("x"))
	_ = sb.WriteFile(context.Background(), h, "y.txt", strings.NewReader("y"))

	res, _ := toolByName(tools, "list_dir").Call(context.Background(), map[string]any{})
	if res.IsError {
		t.Fatalf("list failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "x.txt") || !strings.Contains(res.Content, "y.txt") {
		t.Errorf("list content = %q", res.Content)
	}
}

func TestSchemasAreObjects(t *testing.T) {
	_, _, tools := newMemTools(t)
	for _, tl := range tools {
		if tl.Schema()["type"] != "object" {
			t.Errorf("tool %s schema type = %v", tl.Name(), tl.Schema()["type"])
		}
	}
}

// TestToolsRegisteredAndDispatched exercises the tools through a real Registry
// exactly as the loop does (Call with timeout wrapping). Write then read is
// sequential because the read depends on the write.
func TestToolsRegisteredAndDispatched(t *testing.T) {
	_, _, tools := newMemTools(t)
	reg := toolruntime.NewRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	ctx := context.Background()
	wr := reg.Call(ctx, "write_file", map[string]any{"path": "f.txt", "content": "data"})
	if wr.IsError {
		t.Errorf("write via registry failed: %s", wr.Content)
	}
	rd := reg.Call(ctx, "read_file", map[string]any{"path": "f.txt"})
	if rd.IsError || rd.Content != "data" {
		t.Errorf("read via registry = %+v", rd)
	}
}
