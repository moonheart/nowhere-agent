package memory

import (
	"context"
	"testing"

	"nowhere-agent/internal/identity"
)

func storeMem(t *testing.T, p *MemPort, m Memory) Memory {
	t.Helper()
	got, err := p.Store(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestStoreAssignsIDAndTimestamp(t *testing.T) {
	p := NewMemPort()
	m := storeMem(t, p, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "likes go"})
	if m.ID == "" {
		t.Error("expected ID assigned")
	}
	if m.CreatedAt.IsZero() {
		t.Error("expected CreatedAt set")
	}
}

func TestRecallScopesIsolation(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	storeMem(t, p, Memory{Scope: identity.UserScope("userA"), Kind: KindFact, Content: "userA secret fact"})
	storeMem(t, p, Memory{Scope: identity.UserScope("userB"), Kind: KindFact, Content: "userB secret fact"})

	// userA recalls only their own scope.
	got, err := p.Recall(ctx, "secret fact", []identity.ScopeRef{identity.UserScope("userA")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "userA secret fact" {
		t.Fatalf("userA recall leaked or missed: %+v", got)
	}
}

func TestRecallIncludesTeamAndSystemScopes(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	storeMem(t, p, Memory{Scope: identity.TeamScope("team1"), Kind: KindFact, Content: "team knowledge"})
	storeMem(t, p, Memory{Scope: identity.SystemScope(), Kind: KindFact, Content: "global knowledge"})
	storeMem(t, p, Memory{Scope: identity.UserScope("other"), Kind: KindFact, Content: "other user"})

	scopes := []identity.ScopeRef{identity.UserScope("u1"), identity.TeamScope("team1"), identity.SystemScope()}
	got, _ := p.Recall(ctx, "knowledge", scopes, 10)
	if len(got) != 2 {
		t.Errorf("expected team+system, got %d: %+v", len(got), got)
	}
}

// TestRecallRequiresMatch pins the PGPort symmetry: with a non-empty query, a
// memory with no keyword overlap (score 0) must not surface — before the fix,
// MemPort returned an arbitrary page of unrelated memories as "matches".
func TestRecallRequiresMatch(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	scope := identity.UserScope("u1")
	storeMem(t, p, Memory{Scope: scope, Kind: KindFact, Content: "golang concurrency primitives"})

	got, err := p.Recall(ctx, "electric unicycles", []identity.ScopeRef{scope}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("no-match recall = %+v, want empty", got)
	}

	// An empty query is not a match query: it must still return the memory.
	got, err = p.Recall(ctx, "", []identity.ScopeRef{scope}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("empty-query recall = %d memories, want 1", len(got))
	}
}

func TestRecallExcludesDeprecated(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	m := storeMem(t, p, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "old fact"})
	if err := p.Deprecate(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Recall(ctx, "fact", []identity.ScopeRef{identity.UserScope("u1")}, 10)
	if len(got) != 0 {
		t.Errorf("deprecated memory recalled: %+v", got)
	}
}

func TestForgetRemovesPermanently(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	m := storeMem(t, p, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "to be forgotten"})
	if err := p.Forget(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := p.ListByScope(ctx, identity.UserScope("u1"))
	if len(got) != 0 {
		t.Errorf("forgotten memory still listed: %+v", got)
	}
}

func TestRecallRanksByRelevance(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	storeMem(t, p, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "golang concurrency channels goroutines"})
	storeMem(t, p, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "unrelated stuff"})

	got, _ := p.Recall(ctx, "golang channels", []identity.ScopeRef{identity.UserScope("u1")}, 10)
	if len(got) == 0 || got[0].Content != "golang concurrency channels goroutines" {
		t.Errorf("relevance ranking wrong: %+v", got)
	}
}

func TestRecallRespectsLimit(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		storeMem(t, p, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "fact"})
	}
	got, _ := p.Recall(ctx, "fact", []identity.ScopeRef{identity.UserScope("u1")}, 2)
	if len(got) != 2 {
		t.Errorf("limit not respected: got %d", len(got))
	}
}

func TestListByScopeIncludesDeprecated(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	m := storeMem(t, p, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "x"})
	p.Deprecate(ctx, m.ID)
	got, _ := p.ListByScope(ctx, identity.UserScope("u1"))
	if len(got) != 1 {
		t.Errorf("ListByScope should include deprecated for dreaming scan, got %d", len(got))
	}
}
