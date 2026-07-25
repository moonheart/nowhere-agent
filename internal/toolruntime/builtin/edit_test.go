package builtin

import (
	"context"
	"io"
	"strings"
	"testing"

	"nowhere-agent/internal/sandbox"
)

// setupEdit makes an edit_file tool over a MemPort seeded with one file.
func setupEdit(t *testing.T, initial string) (*editFileTool, sandbox.Port, sandbox.Handle) {
	t.Helper()
	sb := sandbox.NewMemPort()
	h, err := sb.Create(context.Background(), "s1", sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(context.Background(), h, "f.txt", strings.NewReader(initial)); err != nil {
		t.Fatal(err)
	}
	return &editFileTool{sb: sb, h: h}, sb, h
}

func readFile(t *testing.T, sb sandbox.Port, h sandbox.Handle, path string) string {
	t.Helper()
	rc, err := sb.ReadFile(context.Background(), h, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestEditUniqueReplace(t *testing.T) {
	tool, sb, h := setupEdit(t, "hello world")
	res, err := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "world", "new_string": "there",
	})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v err=%v", res, err)
	}
	if got := readFile(t, sb, h, "f.txt"); got != "hello there" {
		t.Errorf("content = %q", got)
	}
}

func TestEditDeleteWithEmptyNewString(t *testing.T) {
	tool, sb, h := setupEdit(t, "keep DROP keep")
	res, err := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "DROP ", "new_string": "",
	})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v", res)
	}
	if got := readFile(t, sb, h, "f.txt"); got != "keep keep" {
		t.Errorf("content = %q", got)
	}
}

func TestEditNotFound(t *testing.T) {
	tool, _, _ := setupEdit(t, "hello")
	res, _ := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "xyz", "new_string": "q",
	})
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Errorf("want not-found error, got %+v", res)
	}
}

func TestEditAmbiguousRequiresUniqueOrReplaceAll(t *testing.T) {
	tool, _, _ := setupEdit(t, "a a a")
	res, _ := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "a", "new_string": "b",
	})
	if !res.IsError || !strings.Contains(res.Content, "appears 3 times") {
		t.Errorf("want ambiguity error, got %+v", res)
	}
}

func TestEditReplaceAll(t *testing.T) {
	tool, sb, h := setupEdit(t, "a a a")
	res, err := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "a", "new_string": "b", "replace_all": true,
	})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v", res)
	}
	if got := readFile(t, sb, h, "f.txt"); got != "b b b" {
		t.Errorf("content = %q", got)
	}
}

func TestEditEmptyOldStringRejected(t *testing.T) {
	tool, _, _ := setupEdit(t, "x")
	res, _ := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "", "new_string": "y",
	})
	if !res.IsError || !strings.Contains(res.Content, "must not be empty") {
		t.Errorf("want empty-old error, got %+v", res)
	}
}

// TestEditPreservesCRLF: a single-line literal edit must leave the CRLF line
// endings around it byte-for-byte intact.
func TestEditPreservesCRLF(t *testing.T) {
	tool, sb, h := setupEdit(t, "x\r\ny\r\nz")
	res, err := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "y", "new_string": "Y",
	})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v", res)
	}
	if got := readFile(t, sb, h, "f.txt"); got != "x\r\nY\r\nz" {
		t.Errorf("CRLF not preserved: %q", got)
	}
}

// TestEditLFOldStringAgainstCRLFFile: the model sends a multi-line LF old_string
// but the file is CRLF; the fallback matches and keeps CRLF endings.
func TestEditLFOldStringAgainstCRLFFile(t *testing.T) {
	tool, sb, h := setupEdit(t, "x\r\ny\r\nz")
	res, err := tool.Call(context.Background(), map[string]any{
		"path": "f.txt", "old_string": "x\ny", "new_string": "X\nY",
	})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v", res)
	}
	if got := readFile(t, sb, h, "f.txt"); got != "X\r\nY\r\nz" {
		t.Errorf("CRLF fallback wrong: %q", got)
	}
}

func TestEditMissingPathArg(t *testing.T) {
	tool, _, _ := setupEdit(t, "x")
	res, _ := tool.Call(context.Background(), map[string]any{
		"old_string": "x", "new_string": "y",
	})
	if !res.IsError || !strings.Contains(res.Content, "path") {
		t.Errorf("want missing-path error, got %+v", res)
	}
}
