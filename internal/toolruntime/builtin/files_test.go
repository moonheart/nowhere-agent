package builtin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

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

// TestListDirToolEmpty pins the C fix: an empty workspace directory must yield
// a non-empty placeholder, not "" — an empty tool_result serializes to a tool
// message with no content field, which the OpenAI/deepseek gateway 400s on.
func TestListDirToolEmpty(t *testing.T) {
	_, _, tools := newMemTools(t)

	res, _ := toolByName(tools, "list_dir").Call(context.Background(), map[string]any{})
	if res.IsError {
		t.Fatalf("list failed: %s", res.Content)
	}
	if res.Content == "" {
		t.Error("empty directory must not produce empty content")
	}
	if !strings.Contains(res.Content, "empty") {
		t.Errorf("expected an empty-directory note, got %q", res.Content)
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

// setupRead makes a read_file tool over a MemPort seeded with one file (T8).
func setupRead(t *testing.T, path, content string) *fileReadTool {
	t.Helper()
	sb := sandbox.NewMemPort()
	h, err := sb.Create(context.Background(), "s1", sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(context.Background(), h, path, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	return &fileReadTool{sb: sb, h: h}
}

// TestReadPaginatesLargeFile pins T8: a file larger than one page returns the
// first page plus a continuation marker, and reading from that marker's offset
// returns the rest with no marker — no bytes lost, none duplicated.
func TestReadPaginatesLargeFile(t *testing.T) {
	head := strings.Repeat("A", readFilePageBytes)
	tail := strings.Repeat("B", 5000)
	tool := setupRead(t, "big.txt", head+tail)

	res, _ := tool.Call(context.Background(), map[string]any{"path": "big.txt"})
	if res.IsError {
		t.Fatalf("first read errored: %s", res.Content)
	}
	if !strings.Contains(res.Content, fmt.Sprintf("offset=%d", readFilePageBytes)) {
		t.Errorf("first page missing continuation marker for offset=%d", readFilePageBytes)
	}
	body, _, found := strings.Cut(res.Content, "\n\n[read_file:")
	if !found {
		t.Fatal("first page had no marker to cut on")
	}
	if body != head {
		t.Errorf("first page body = %d bytes, want %d", len(body), len(head))
	}

	res2, _ := tool.Call(context.Background(), map[string]any{"path": "big.txt", "offset": readFilePageBytes})
	if res2.IsError {
		t.Fatalf("second read errored: %s", res2.Content)
	}
	if res2.Content != tail {
		t.Errorf("continuation length = %d, want %d (and no marker)", len(res2.Content), len(tail))
	}
}

// TestReadHonorsExplicitLimit: a caller-supplied limit bounds the page and the
// marker points to the byte after it.
func TestReadHonorsExplicitLimit(t *testing.T) {
	tool := setupRead(t, "big.txt", strings.Repeat("A", 100))
	res, _ := tool.Call(context.Background(), map[string]any{"path": "big.txt", "limit": 10})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	body, _, found := strings.Cut(res.Content, "\n\n[read_file:")
	if !found {
		t.Fatal("expected a continuation marker for a limited read")
	}
	if body != strings.Repeat("A", 10) {
		t.Errorf("limited page = %d bytes, want 10", len(body))
	}
	if !strings.Contains(res.Content, "offset=10") {
		t.Errorf("want offset=10 marker, got tail %q", res.Content[len(body):])
	}
}

// TestReadOffsetPastEndOfFile: an offset past EOF reports the size rather than
// returning an empty result, and is not an error.
func TestReadOffsetPastEndOfFile(t *testing.T) {
	tool := setupRead(t, "s.txt", "short")
	res, _ := tool.Call(context.Background(), map[string]any{"path": "s.txt", "offset": 100})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "past end of file") || !strings.Contains(res.Content, "5 bytes") {
		t.Errorf("want past-EOF note with size, got %q", res.Content)
	}
}

// TestReadPreservesBytesAndReturnsSmallFileWhole: a sub-page file comes back
// byte-exact (CRLF intact) with no marker — the pre-T8 behaviour for small reads.
func TestReadPreservesBytesAndReturnsSmallFileWhole(t *testing.T) {
	tool := setupRead(t, "c.txt", "a\r\nb\r\nc")
	res, _ := tool.Call(context.Background(), map[string]any{"path": "c.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Content != "a\r\nb\r\nc" {
		t.Errorf("small file not returned byte-exact: %q", res.Content)
	}
}

// markerOffset extracts the integer following "offset=" in a continuation or
// spill marker (e.g. "... offset=19998 to continue]" or "... offset=17998)]").
func markerOffset(t *testing.T, s string) int {
	t.Helper()
	_, after, ok := strings.Cut(s, "offset=")
	if !ok {
		t.Fatalf("no offset= in marker %q", s)
	}
	n := 0
	for n < len(after) && after[n] >= '0' && after[n] <= '9' {
		n++
	}
	v, err := strconv.Atoi(after[:n])
	if err != nil {
		t.Fatalf("bad offset in marker %q: %v", s, err)
	}
	return v
}

// TestReadPagesUTF8OnRuneBoundaries pins the rune-safety fix: paging a file of
// 3-byte runes (whose length no page size divides evenly) following the tool's
// own continuation offsets yields pages that are each valid UTF-8, with no split
// runes / U+FFFD at the seams, and reassemble to the exact original.
func TestReadPagesUTF8OnRuneBoundaries(t *testing.T) {
	content := strings.Repeat("汉", 8000) // 24000 bytes; 20000-byte pages land mid-rune
	tool := setupRead(t, "u.txt", content)

	var got strings.Builder
	offset := 0
	for i := 0; i < 100; i++ { // bounded so a pagination bug fails instead of hanging
		res, _ := tool.Call(context.Background(), map[string]any{"path": "u.txt", "offset": offset})
		if res.IsError {
			t.Fatalf("read at offset %d errored: %s", offset, res.Content)
		}
		body, marker, hasMarker := strings.Cut(res.Content, "\n\n[read_file:")
		if !utf8.ValidString(body) || strings.ContainsRune(body, '�') {
			t.Fatalf("page at offset %d split a rune (invalid UTF-8)", offset)
		}
		got.WriteString(body)
		if !hasMarker {
			break
		}
		next := markerOffset(t, marker)
		if next <= offset {
			t.Fatalf("continuation offset %d did not advance past %d", next, offset)
		}
		offset = next
	}
	if got.String() != content {
		t.Errorf("reassembled %d bytes, want %d — pagination lost or duplicated content", got.Len(), len(content))
	}
}

// TestReadAlignsHandPickedMidRuneOffset: a caller-supplied offset that lands
// inside a rune retreats to the rune start rather than emitting a leading
// U+FFFD, so the page still begins with a whole character.
func TestReadAlignsHandPickedMidRuneOffset(t *testing.T) {
	tool := setupRead(t, "u.txt", strings.Repeat("汉", 10)) // 30 bytes, one page
	res, _ := tool.Call(context.Background(), map[string]any{"path": "u.txt", "offset": 1})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.ContainsRune(res.Content, '�') || !utf8.ValidString(res.Content) {
		t.Errorf("mid-rune offset produced a split rune: %d bytes", len(res.Content))
	}
	if !strings.HasPrefix(res.Content, "汉") {
		t.Error("retreated page should start on the rune boundary (a whole 汉)")
	}
}
