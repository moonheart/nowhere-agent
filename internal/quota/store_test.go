package quota

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// These run against the real dev Postgres (see store_test.go convention in the
// usage package): unique random owner ids per test, delete only the rows we
// create by id, never an unscoped DELETE. The usage_budgets migration must be
// applied (go run ./cmd/migrate); without a reachable Postgres the tests skip.

func testDSN() string {
	if v := os.Getenv("QUOTA_PG_TEST_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
}

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testDSN())
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

func randOwner() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "qt-" + hex.EncodeToString(b)
}

// cleanup removes only the (scope, owner) rows this test created.
func cleanup(t *testing.T, db *sql.DB, scope Scope, owners ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, o := range owners {
			_, _ = db.Exec(`DELETE FROM usage_budgets WHERE scope = $1 AND owner_id = $2`, string(scope), o)
		}
	})
}

func TestStoreSetGetRoundTrip(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	owner := randOwner()
	cleanup(t, db, ScopeUser, owner)

	if err := s.Set(ctx, ScopeUser, owner, 12345); err != nil {
		t.Fatalf("Set: %v", err)
	}
	b, ok, err := s.Get(ctx, ScopeUser, owner)
	if err != nil || !ok {
		t.Fatalf("Get after Set: ok=%v err=%v", ok, err)
	}
	if b.Scope != ScopeUser || b.OwnerID != owner || b.MonthlyTokens != 12345 {
		t.Fatalf("round trip mismatch: %+v", b)
	}
	if b.UpdatedAt.IsZero() {
		t.Fatal("updated_at should be set")
	}
}

func TestStoreSetUpserts(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	owner := randOwner()
	cleanup(t, db, ScopeTeam, owner)

	if err := s.Set(ctx, ScopeTeam, owner, 100); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(ctx, ScopeTeam, owner, 200); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	b, _, _ := s.Get(ctx, ScopeTeam, owner)
	if b.MonthlyTokens != 200 {
		t.Fatalf("upsert should overwrite, got %d", b.MonthlyTokens)
	}
}

func TestStoreSetRejectsNonPositive(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	owner := randOwner()
	cleanup(t, db, ScopeUser, owner)

	for _, n := range []int64{0, -5} {
		if err := s.Set(ctx, ScopeUser, owner, n); err == nil {
			t.Fatalf("Set(%d) should be rejected", n)
		}
	}
	// And nothing was stored.
	if _, ok, _ := s.Get(ctx, ScopeUser, owner); ok {
		t.Fatal("rejected sets must not leave a row")
	}
}

func TestStoreGetAbsent(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	if _, ok, err := s.Get(context.Background(), ScopeUser, randOwner()); err != nil || ok {
		t.Fatalf("absent budget should be (false, nil), got ok=%v err=%v", ok, err)
	}
}

func TestStoreClear(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	owner := randOwner()
	cleanup(t, db, ScopeUser, owner)

	// Clearing an absent budget reports false.
	if cleared, err := s.Clear(ctx, ScopeUser, owner); err != nil || cleared {
		t.Fatalf("clear absent should be (false, nil), got cleared=%v err=%v", cleared, err)
	}
	if err := s.Set(ctx, ScopeUser, owner, 100); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if cleared, err := s.Clear(ctx, ScopeUser, owner); err != nil || !cleared {
		t.Fatalf("clear present should be (true, nil), got cleared=%v err=%v", cleared, err)
	}
	if _, ok, _ := s.Get(ctx, ScopeUser, owner); ok {
		t.Fatal("cleared budget should be gone")
	}
}

func TestStoreScopesAreIndependent(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	owner := randOwner()
	cleanup(t, db, ScopeUser, owner)
	cleanup(t, db, ScopeTeam, owner)

	// Same owner id, different scopes: two independent rows.
	if err := s.Set(ctx, ScopeUser, owner, 1); err != nil {
		t.Fatalf("Set user: %v", err)
	}
	if err := s.Set(ctx, ScopeTeam, owner, 2); err != nil {
		t.Fatalf("Set team: %v", err)
	}
	ub, _, _ := s.Get(ctx, ScopeUser, owner)
	tb, _, _ := s.Get(ctx, ScopeTeam, owner)
	if ub.MonthlyTokens != 1 || tb.MonthlyTokens != 2 {
		t.Fatalf("scopes should hold independent budgets, got user=%d team=%d", ub.MonthlyTokens, tb.MonthlyTokens)
	}
}
