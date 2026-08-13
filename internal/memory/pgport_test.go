package memory

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/identity"
)

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := getenvOr("MEMORY_PG_TEST_DSN", "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// hasVector reports whether the pgvector extension is installed.
func hasVector(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='vector')`).Scan(&ok); err != nil {
		t.Fatalf("check vector ext: %v", err)
	}
	return ok
}

// cleanup removes the memories a test created.
func cleanup(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			db.Exec(`DELETE FROM memories WHERE id = $1`, id)
		}
	})
}

func TestPGPortStoreAndListByScope(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-pgport")

	m, err := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "user likes Go"})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	cleanup(t, db, m.ID)
	if m.ID == "" || m.CreatedAt.IsZero() {
		t.Errorf("store did not assign id/timestamps: %+v", m)
	}

	got, err := p.ListByScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListByScope: %v", err)
	}
	if len(got) != 1 || got[0].Content != "user likes Go" {
		t.Errorf("ListByScope = %+v", got)
	}

	// Other scopes must not see it (isolation).
	other, _ := p.ListByScope(ctx, identity.UserScope("someone-else"))
	if len(other) != 0 {
		t.Errorf("scope isolation violated: %+v", other)
	}
}

func TestPGPortRecallKeywordRanks(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-recall")

	a, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "prefers dark mode in the editor"})
	b, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "favorite language is golang"})
	cleanup(t, db, a.ID, b.ID)

	got, err := p.Recall(ctx, "golang language", []identity.ScopeRef{scope}, 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no recall results")
	}
	if got[0].ID != b.ID {
		t.Errorf("expected golang memory ranked first, got %+v", got)
	}
}

// TestPGPortRecallRequiresMatch pins the relevance floor: a query with no
// keyword overlap must return nothing, not an arbitrary page of unrelated
// memories ordered by whatever ts_rank tied at zero.
func TestPGPortRecallRequiresMatch(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-nomatch")

	m, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "prefers dark mode in the editor"})
	cleanup(t, db, m.ID)

	got, err := p.Recall(ctx, "nonexistentkeywordzzz", []identity.ScopeRef{scope}, 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("no-match recall returned %d unrelated memories, want 0: %+v", len(got), got)
	}
	got, err = p.RecallSince(ctx, time.Time{}, "nonexistentkeywordzzz", []identity.ScopeRef{scope}, nil, 10)
	if err != nil {
		t.Fatalf("RecallSince: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("no-match recall-since returned %d unrelated memories, want 0: %+v", len(got), got)
	}
}

func TestPGPortRecallExcludesDeprecated(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-depr")

	m, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "old fact golang"})
	cleanup(t, db, m.ID)
	if err := p.Deprecate(ctx, m.ID); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}

	got, _ := p.Recall(ctx, "golang", []identity.ScopeRef{scope}, 10)
	for _, r := range got {
		if r.ID == m.ID {
			t.Error("deprecated memory returned by Recall")
		}
	}
	// But still listed for auditing.
	listed, _ := p.ListByScope(ctx, scope)
	found := false
	for _, r := range listed {
		if r.ID == m.ID && r.Deprecated {
			found = true
		}
	}
	if !found {
		t.Error("deprecated memory missing from ListByScope")
	}
}

func TestPGPortForget(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-forget")

	m, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "to be forgotten"})
	if err := p.Forget(ctx, m.ID); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	got, _ := p.ListByScope(ctx, scope)
	for _, r := range got {
		if r.ID == m.ID {
			t.Error("forgotten memory still present")
		}
	}
}

func TestPGPortVectorRecall(t *testing.T) {
	db := pgTestDB(t)
	if !hasVector(t, db) {
		t.Skip("pgvector extension not installed")
	}
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-vector")

	// Two 3-dim embeddings pointing in different directions.
	near, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "near", Embedding: []float32{1, 0, 0}})
	far, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "far", Embedding: []float32{0, 1, 0}})
	cleanup(t, db, near.ID, far.ID)

	got, err := p.RecallVector(ctx, []float32{1, 0, 0}, []identity.ScopeRef{scope}, 10)
	if err != nil {
		t.Fatalf("RecallVector: %v", err)
	}
	if len(got) == 0 || got[0].ID != near.ID {
		t.Errorf("expected nearest memory first, got %+v", got)
	}
	// Embedding should round-trip.
	if len(got[0].Embedding) != 3 || got[0].Embedding[0] != 1 {
		t.Errorf("embedding round-trip = %v", got[0].Embedding)
	}
}

func TestPGPortEmbeddingRoundTrip(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("user-emb")

	m, _ := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "emb", Embedding: []float32{0.5, -1.25, 3}})
	cleanup(t, db, m.ID)

	got, _ := p.ListByScope(ctx, scope)
	if len(got) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(got))
	}
	want := []float32{0.5, -1.25, 3}
	if len(got[0].Embedding) != len(want) {
		t.Fatalf("embedding len = %d want %d", len(got[0].Embedding), len(want))
	}
	for i := range want {
		if got[0].Embedding[i] != want[i] {
			t.Errorf("embedding[%d] = %v want %v", i, got[0].Embedding[i], want[i])
		}
	}
}
