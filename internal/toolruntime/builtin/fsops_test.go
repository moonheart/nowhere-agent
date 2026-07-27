package builtin

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/toolruntime"
)

func TestFSOpsToolsProvided(t *testing.T) {
	_, _, tools := newMemTools(t)
	for _, want := range []string{"move_file", "copy_file", "delete_file", "make_dir"} {
		if toolByName(tools, want) == nil {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestFSOpsRiskClassification(t *testing.T) {
	_, _, tools := newMemTools(t)
	for _, name := range []string{"move_file", "copy_file", "delete_file", "make_dir"} {
		if r := toolByName(tools, name).Risk(); r != toolruntime.RiskSandboxWrite {
			t.Errorf("%s risk = %q, want sandbox_write", name, r)
		}
	}
}

func write(t *testing.T, tools []toolruntime.Tool, path, content string) {
	t.Helper()
	res, _ := toolByName(tools, "write_file").Call(context.Background(), map[string]any{
		"path": path, "content": content,
	})
	if res.IsError {
		t.Fatalf("write %s failed: %s", path, res.Content)
	}
}

func read(t *testing.T, tools []toolruntime.Tool, path string) (string, bool) {
	t.Helper()
	res, _ := toolByName(tools, "read_file").Call(context.Background(), map[string]any{"path": path})
	if res.IsError {
		return "", false
	}
	return res.Content, true
}

func TestMoveFileThenReadBack(t *testing.T) {
	_, _, tools := newMemTools(t)
	write(t, tools, "a.txt", "movable")

	mv, _ := toolByName(tools, "move_file").Call(context.Background(), map[string]any{
		"src": "a.txt", "dst": "sub/b.txt",
	})
	if mv.IsError {
		t.Fatalf("move failed: %s", mv.Content)
	}
	if _, ok := read(t, tools, "a.txt"); ok {
		t.Error("source should be gone after move")
	}
	if got, ok := read(t, tools, "sub/b.txt"); !ok || got != "movable" {
		t.Errorf("dst read = %q, %v", got, ok)
	}
}

func TestCopyFileKeepsBoth(t *testing.T) {
	_, _, tools := newMemTools(t)
	write(t, tools, "orig.txt", "dup me")

	cp, _ := toolByName(tools, "copy_file").Call(context.Background(), map[string]any{
		"src": "orig.txt", "dst": "copy.txt",
	})
	if cp.IsError {
		t.Fatalf("copy failed: %s", cp.Content)
	}
	if got, ok := read(t, tools, "orig.txt"); !ok || got != "dup me" {
		t.Errorf("source changed: %q, %v", got, ok)
	}
	if got, ok := read(t, tools, "copy.txt"); !ok || got != "dup me" {
		t.Errorf("copy read = %q, %v", got, ok)
	}
}

func TestCopyDirectoryRecursive(t *testing.T) {
	_, _, tools := newMemTools(t)
	write(t, tools, "dir/one.txt", "1")
	write(t, tools, "dir/nested/two.txt", "2")

	cp, _ := toolByName(tools, "copy_file").Call(context.Background(), map[string]any{
		"src": "dir", "dst": "dir2",
	})
	if cp.IsError {
		t.Fatalf("copy dir failed: %s", cp.Content)
	}
	for path, want := range map[string]string{"dir2/one.txt": "1", "dir2/nested/two.txt": "2"} {
		if got, ok := read(t, tools, path); !ok || got != want {
			t.Errorf("%s = %q, %v; want %q", path, got, ok, want)
		}
	}
}

func TestMoveDirectoryRecursive(t *testing.T) {
	_, _, tools := newMemTools(t)
	write(t, tools, "src/x.txt", "x")
	write(t, tools, "src/inner/y.txt", "y")

	mv, _ := toolByName(tools, "move_file").Call(context.Background(), map[string]any{
		"src": "src", "dst": "dst",
	})
	if mv.IsError {
		t.Fatalf("move dir failed: %s", mv.Content)
	}
	if _, ok := read(t, tools, "src/x.txt"); ok {
		t.Error("src/x.txt should be gone after directory move")
	}
	if got, ok := read(t, tools, "dst/inner/y.txt"); !ok || got != "y" {
		t.Errorf("dst/inner/y.txt = %q, %v", got, ok)
	}
}

func TestDeleteFileNoFlag(t *testing.T) {
	_, _, tools := newMemTools(t)
	write(t, tools, "gone.txt", "bye")

	del, _ := toolByName(tools, "delete_file").Call(context.Background(), map[string]any{"path": "gone.txt"})
	if del.IsError {
		t.Fatalf("delete file failed: %s", del.Content)
	}
	if _, ok := read(t, tools, "gone.txt"); ok {
		t.Error("file should be deleted")
	}
}

func TestDeleteDirectoryRequiresRecursive(t *testing.T) {
	_, _, tools := newMemTools(t)
	write(t, tools, "tree/a.txt", "a")

	// Without recursive: refused, content intact.
	del, _ := toolByName(tools, "delete_file").Call(context.Background(), map[string]any{"path": "tree"})
	if !del.IsError {
		t.Error("expected IsError deleting a directory without recursive")
	}
	if _, ok := read(t, tools, "tree/a.txt"); !ok {
		t.Error("directory should be intact after refused delete")
	}

	// With recursive: deletes the whole tree.
	del, _ = toolByName(tools, "delete_file").Call(context.Background(), map[string]any{
		"path": "tree", "recursive": true,
	})
	if del.IsError {
		t.Fatalf("recursive delete failed: %s", del.Content)
	}
	if _, ok := read(t, tools, "tree/a.txt"); ok {
		t.Error("tree/a.txt should be deleted after recursive delete")
	}
}

func TestMakeDir(t *testing.T) {
	_, _, tools := newMemTools(t)
	res, _ := toolByName(tools, "make_dir").Call(context.Background(), map[string]any{"path": "a/b/c"})
	if res.IsError {
		t.Fatalf("make_dir failed: %s", res.Content)
	}
}

func TestFSOpsMissingArg(t *testing.T) {
	_, _, tools := newMemTools(t)
	res, _ := toolByName(tools, "move_file").Call(context.Background(), map[string]any{"src": "a"})
	if !res.IsError || !strings.Contains(res.Content, "dst") {
		t.Errorf("expected missing-dst error, got %+v", res)
	}
}

func TestMoveNonexistentIsError(t *testing.T) {
	_, _, tools := newMemTools(t)
	res, _ := toolByName(tools, "move_file").Call(context.Background(), map[string]any{
		"src": "nope.txt", "dst": "x.txt",
	})
	if !res.IsError {
		t.Error("expected IsError moving a nonexistent path")
	}
}
