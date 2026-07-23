package toolruntime

import (
	"context"
	"sort"
	"testing"
	"time"
)

// stubTool is a minimal Tool for registry tests.
type stubTool struct{ name string }

func (s stubTool) Name() string                                         { return s.name }
func (s stubTool) Description() string                                  { return "" }
func (s stubTool) Schema() map[string]any                               { return map[string]any{} }
func (s stubTool) Risk() Risk                                           { return RiskReadOnly }
func (s stubTool) Timeout() time.Duration                               { return 0 }
func (s stubTool) Call(context.Context, map[string]any) (Result, error) { return Result{}, nil }

func newTestRegistry(names ...string) *Registry {
	r := NewRegistry()
	for _, n := range names {
		r.Register(stubTool{name: n})
	}
	return r
}

func names(r *Registry) []string {
	out := r.Names()
	sort.Strings(out)
	return out
}

func TestScopedWildcard(t *testing.T) {
	r := newTestRegistry("read_file", "write_file", "spawn_agent")
	v := r.Scoped(nil, nil)
	got := names(v)
	if len(got) != 3 {
		t.Fatalf("wildcard view should have all tools, got %v", got)
	}
}

func TestScopedAllowList(t *testing.T) {
	r := newTestRegistry("read_file", "write_file", "list_dir")
	v := r.Scoped([]string{"read_file", "list_dir", "nonexistent"}, nil)
	got := names(v)
	if len(got) != 2 || got[0] != "list_dir" || got[1] != "read_file" {
		t.Fatalf("allow-list view: %v", got)
	}
}

func TestScopedDenyAndExclude(t *testing.T) {
	r := newTestRegistry("read_file", "write_file", "spawn_agent")
	v := r.Scoped(nil, []string{"write_file"}, "spawn_agent")
	got := names(v)
	if len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("deny+exclude view: %v", got)
	}
}

func TestScopedStarWildcard(t *testing.T) {
	r := newTestRegistry("read_file", "write_file")
	v := r.Scoped([]string{"*"}, []string{"write_file"})
	got := names(v)
	if len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("star wildcard minus deny: %v", got)
	}
}

func TestScopedDoesNotMutateParent(t *testing.T) {
	r := newTestRegistry("read_file", "write_file", "spawn_agent")
	_ = r.Scoped([]string{"read_file"}, nil, "spawn_agent")
	if len(names(r)) != 3 {
		t.Fatalf("parent registry mutated: %v", names(r))
	}
}
