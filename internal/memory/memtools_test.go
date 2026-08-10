package memory

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
)

func TestWriteMemoryToolStoresUserMemory(t *testing.T) {
	p := NewMemPort()
	tool := NewWriteMemoryTool(p, "u1")

	res, err := tool.Call(context.Background(), map[string]any{
		"kind":    "preference",
		"content": "prefers Chinese replies",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("write failed: %+v", res)
	}
	if !strings.Contains(res.Content, "prefers Chinese replies") {
		t.Errorf("result missing content: %q", res.Content)
	}

	// The stored memory must be user-scoped to u1 and immediately recallable.
	got, err := p.Recall(context.Background(), "language", []identity.ScopeRef{identity.UserScope("u1")}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "prefers Chinese replies" || got[0].Kind != KindPreference {
		t.Errorf("stored memory = %+v", got)
	}
	// And not visible to another user.
	other, _ := p.Recall(context.Background(), "language", []identity.ScopeRef{identity.UserScope("u2")}, 5)
	if len(other) != 0 {
		t.Errorf("memory leaked across users: %+v", other)
	}
}

func TestWriteMemoryToolValidatesArgs(t *testing.T) {
	tool := NewWriteMemoryTool(NewMemPort(), "u1")
	if res, _ := tool.Call(context.Background(), map[string]any{"kind": "bogus", "content": "x"}); !res.IsError {
		t.Errorf("bad kind should fail, got %+v", res)
	}
	if res, _ := tool.Call(context.Background(), map[string]any{"kind": "fact", "content": "  "}); !res.IsError {
		t.Errorf("blank content should fail, got %+v", res)
	}
}

func TestEditMemoryToolUpdatesOwnMemory(t *testing.T) {
	p := NewMemPort()
	scope := identity.UserScope("u1")
	m, err := p.Store(context.Background(), Memory{Scope: scope, Kind: KindFact, Content: "old content"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewEditMemoryTool(p, "u1")

	res, err := tool.Call(context.Background(), map[string]any{"id": m.ID, "content": "new content"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("edit failed: %+v", res)
	}
	got, err := p.GetByID(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "new content" {
		t.Errorf("content after edit = %q", got.Content)
	}
	if got.Kind != KindFact {
		t.Errorf("kind must be unchanged, got %q", got.Kind)
	}
}

func TestEditMemoryToolDeniesForeignMemory(t *testing.T) {
	p := NewMemPort()
	m, err := p.Store(context.Background(), Memory{Scope: identity.UserScope("u2"), Kind: KindFact, Content: "u2's"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewEditMemoryTool(p, "u1")
	res, _ := tool.Call(context.Background(), map[string]any{"id": m.ID, "content": "hijacked"})
	if !res.IsError {
		t.Errorf("editing another user's memory must fail, got %+v", res)
	}
	got, _ := p.GetByID(context.Background(), m.ID)
	if got.Content != "u2's" {
		t.Errorf("foreign memory was modified: %q", got.Content)
	}
}

func TestEditMemoryToolMissingID(t *testing.T) {
	tool := NewEditMemoryTool(NewMemPort(), "u1")
	res, _ := tool.Call(context.Background(), map[string]any{"id": "no-such-id", "content": "x"})
	if !res.IsError {
		t.Errorf("missing id should fail, got %+v", res)
	}
	if !strings.Contains(res.Content, "no memory with id") {
		t.Errorf("missing-id result = %q", res.Content)
	}
}

func TestForgetMemoryToolDeletesOwnMemory(t *testing.T) {
	p := NewMemPort()
	m, err := p.Store(context.Background(), Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "gone soon"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewForgetMemoryTool(p, "u1")

	res, err := tool.Call(context.Background(), map[string]any{"id": m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("forget failed: %+v", res)
	}
	if _, err := p.GetByID(context.Background(), m.ID); err == nil {
		t.Error("memory still exists after forget")
	}
}

func TestForgetMemoryToolDeniesForeignMemory(t *testing.T) {
	p := NewMemPort()
	m, _ := p.Store(context.Background(), Memory{Scope: identity.UserScope("u2"), Kind: KindFact, Content: "u2's"})
	tool := NewForgetMemoryTool(p, "u1")
	res, _ := tool.Call(context.Background(), map[string]any{"id": m.ID})
	if !res.IsError {
		t.Errorf("deleting another user's memory must fail, got %+v", res)
	}
	if _, err := p.GetByID(context.Background(), m.ID); err != nil {
		t.Error("foreign memory was deleted")
	}
}

// TestRecallToolOutputCarriesID pins that recall results include the memory's
// id, so the model can reference it with edit_memory / forget_memory.
func TestRecallToolOutputCarriesID(t *testing.T) {
	p := NewMemPort()
	m, err := p.Store(context.Background(), Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "golang fact"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewRecallTool(p, []identity.ScopeRef{identity.UserScope("u1")})
	res, err := tool.Call(context.Background(), map[string]any{"query": "golang", "kinds": []any{"fact"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, m.ID) {
		t.Errorf("recall output missing memory id %s: %q", m.ID, res.Content)
	}
}
