package memory

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
)

// storeAt stores a memory with a controlled CreatedAt (Store would overwrite it
// with now) so incremental-recall tests can order memories in time.
func storeAt(t *testing.T, p *MemPort, m Memory, at time.Time) Memory {
	t.Helper()
	got, err := p.Store(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.memories[got.ID].CreatedAt = at
	p.mu.Unlock()
	got.CreatedAt = at
	return got
}

func TestRecallSinceFiltersByTime(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	scope := identity.UserScope("u1")
	base := time.Now().Add(-time.Hour)

	old := storeAt(t, p, Memory{Scope: scope, Kind: KindFact, Content: "old golang fact"}, base)
	recent := storeAt(t, p, Memory{Scope: scope, Kind: KindFact, Content: "new golang fact"}, base.Add(30*time.Minute))

	// since=zero → both.
	got, _ := p.RecallSince(ctx, time.Time{}, "golang", []identity.ScopeRef{scope}, nil, 10)
	if len(got) != 2 {
		t.Fatalf("zero-since recall = %d want 2", len(got))
	}

	// since after old → only the recent one.
	got, _ = p.RecallSince(ctx, base.Add(10*time.Minute), "golang", []identity.ScopeRef{scope}, nil, 10)
	if len(got) != 1 || got[0].ID != recent.ID {
		t.Fatalf("incremental recall = %+v want only the recent memory", got)
	}
	_ = old
}

func TestRecallSinceFiltersByKind(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	scope := identity.UserScope("u1")

	storeAt(t, p, Memory{Scope: scope, Kind: KindFact, Content: "a fact"}, time.Now())
	storeAt(t, p, Memory{Scope: scope, Kind: KindSummary, Content: "a summary"}, time.Now())

	got, _ := p.RecallSince(ctx, time.Time{}, "", []identity.ScopeRef{scope}, []Kind{KindSummary}, 10)
	if len(got) != 1 || got[0].Kind != KindSummary {
		t.Fatalf("kind-filtered recall = %+v want only the summary", got)
	}
}

func TestRecallSinceExcludesDeprecated(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	scope := identity.UserScope("u1")

	m := storeAt(t, p, Memory{Scope: scope, Kind: KindFact, Content: "doomed"}, time.Now())
	if err := p.Deprecate(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := p.RecallSince(ctx, time.Time{}, "doomed", []identity.ScopeRef{scope}, nil, 10)
	if len(got) != 0 {
		t.Errorf("deprecated memory must not be recalled, got %+v", got)
	}
}

func TestRecallSinceEmptyQueryRecencyOrder(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	scope := identity.UserScope("u1")
	base := time.Now().Add(-time.Hour)

	older := storeAt(t, p, Memory{Scope: scope, Kind: KindFact, Content: "older"}, base)
	newer := storeAt(t, p, Memory{Scope: scope, Kind: KindFact, Content: "newer"}, base.Add(time.Minute))

	got, _ := p.RecallSince(ctx, time.Time{}, "", []identity.ScopeRef{scope}, nil, 10)
	if len(got) != 2 || got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Fatalf("empty-query recall should be recency-ordered, got %+v", got)
	}
}
