package builtin

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/sandbox"
)

func setupSearch(t *testing.T, files map[string]string) (sandbox.Port, sandbox.Handle) {
	t.Helper()
	sb := sandbox.NewMemPort()
	h, err := sb.Create(context.Background(), "s", sandbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		if err := sb.WriteFile(context.Background(), h, p, strings.NewReader(c)); err != nil {
			t.Fatal(err)
		}
	}
	return sb, h
}

func TestGrepFindsMatchesWithLineNumbers(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{
		"a.go":  "package main\nfunc Foo() {}\n",
		"b.txt": "nothing\nFoo again\n",
	})
	res, err := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "Foo"})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v", res)
	}
	if !strings.Contains(res.Content, "a.go:2:func Foo() {}") {
		t.Errorf("missing a.go match: %q", res.Content)
	}
	if !strings.Contains(res.Content, "b.txt:2:Foo again") {
		t.Errorf("missing b.txt match: %q", res.Content)
	}
}

func TestGrepGlobFilter(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{
		"a.go":  "Foo\n",
		"b.txt": "Foo\n",
	})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "Foo", "glob": "*.go"})
	if strings.Contains(res.Content, "b.txt") {
		t.Errorf("glob filter leaked b.txt: %q", res.Content)
	}
	if !strings.Contains(res.Content, "a.go") {
		t.Errorf("glob filter dropped a.go: %q", res.Content)
	}
}

func TestGrepPathScope(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{
		"src/x.go": "Foo\n",
		"y.go":     "Foo\n",
	})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "Foo", "path": "src"})
	if strings.Contains(res.Content, "y.go") {
		t.Errorf("path scope leaked y.go: %q", res.Content)
	}
	if !strings.Contains(res.Content, "src/x.go") {
		t.Errorf("path scope dropped src/x.go: %q", res.Content)
	}
}

func TestGrepNoMatches(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{"a.txt": "hello\n"})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "zzz"})
	if res.IsError || res.Content != "(no matches)" {
		t.Errorf("want (no matches), got %+v", res)
	}
}

func TestGrepMaxResultsTruncates(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{"a.txt": "Foo\nFoo\nFoo\nFoo\n"})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "Foo", "max_results": float64(2)})
	if strings.Count(res.Content, "a.txt:") != 2 {
		t.Errorf("want 2 matches, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "stopped at 2") {
		t.Errorf("missing truncation note: %q", res.Content)
	}
}

func TestGrepSkipsBinary(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{"bin": "a\x00Foo\n"})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "Foo"})
	if res.Content != "(no matches)" {
		t.Errorf("binary file should be skipped, got %q", res.Content)
	}
}

func TestGrepTrimsCR(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{"a.txt": "l1\r\nFoo\r\n"})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "Foo"})
	if !strings.Contains(res.Content, "a.txt:2:Foo") || strings.Contains(res.Content, "Foo\r") {
		t.Errorf("CR not trimmed from match text: %q", res.Content)
	}
}

func TestGrepInvalidPattern(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{"a.txt": "x"})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "("})
	if !res.IsError || !strings.Contains(res.Content, "invalid pattern") {
		t.Errorf("want invalid-pattern error, got %+v", res)
	}
}

func TestGlobMatches(t *testing.T) {
	sb, h := setupSearch(t, map[string]string{
		"a.go":     "x",
		"sub/b.go": "x",
		"c.txt":    "x",
	})
	res, err := (&globTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "**/*.go"})
	if err != nil || res.IsError {
		t.Fatalf("unexpected: %+v", res)
	}
	if !strings.Contains(res.Content, "a.go") || !strings.Contains(res.Content, "sub/b.go") {
		t.Errorf("missing go files: %q", res.Content)
	}
	if strings.Contains(res.Content, "c.txt") {
		t.Errorf("glob leaked c.txt: %q", res.Content)
	}
}

func TestSearchToolsUnsupportedBackend(t *testing.T) {
	// noWalkPort embeds the Port interface, so it inherits Create/ReadFile/... but
	// NOT Walk — exercising the tools' graceful "not supported" path.
	sb := noWalkPort{sandbox.NewMemPort()}
	h, _ := sb.Create(context.Background(), "s", sandbox.Options{})
	res, _ := (&grepTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "x"})
	if !res.IsError || !strings.Contains(res.Content, "not supported") {
		t.Errorf("grep want not-supported, got %+v", res)
	}
	res, _ = (&globTool{sb: sb, h: h}).Call(context.Background(), map[string]any{"pattern": "*"})
	if !res.IsError || !strings.Contains(res.Content, "not supported") {
		t.Errorf("glob want not-supported, got %+v", res)
	}
}

type noWalkPort struct{ sandbox.Port }

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		glob  string
		match []string
		no    []string
	}{
		{"**/*.go", []string{"main.go", "a/b/main.go"}, []string{"main.py", "go"}},
		{"*.go", []string{"main.go"}, []string{"a/main.go"}},
		{"src/**/test_*.py", []string{"src/test_x.py", "src/a/test_y.py"}, []string{"src/a/x.py", "test_x.py"}},
		{"a?c", []string{"abc", "axc"}, []string{"ac", "a/c"}},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.glob)
		if err != nil {
			t.Fatalf("glob %q: %v", c.glob, err)
		}
		for _, m := range c.match {
			if !re.MatchString(m) {
				t.Errorf("glob %q should match %q", c.glob, m)
			}
		}
		for _, n := range c.no {
			if re.MatchString(n) {
				t.Errorf("glob %q should NOT match %q", c.glob, n)
			}
		}
	}
}
