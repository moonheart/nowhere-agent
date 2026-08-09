package agentdef

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/identity"
)

// These tests run against the shared development Postgres (skipping when none
// is reachable), the repo's convention. The test database IS the dev database,
// so every row uses a unique random name and cleanup deletes only the
// definitions this test created, by ID — never an unscoped DELETE/UPDATE.

func agentdefTestDSN() string {
	if v := os.Getenv("AGENTDEF_PG_TEST_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
}

func pgDefDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", agentdefTestDSN())
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

func defSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cleanupDef deletes exactly the agent_defs row (and cascaded versions)
// created by a test. It is scoped by ID, so it can never touch another
// tenant's data.
func cleanupDef(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM agent_defs WHERE id = $1`, id); err != nil {
			t.Logf("cleanup agent def %s: %v", id, err)
		}
	})
}

func defDoc(name, body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: test def %s\ntools: read_file\nskills: lint\n---\n%s\n", name, name, body)
}

func putDef(t *testing.T, st *PGStore, doc string, scope identity.ScopeRef) StoredDef {
	t.Helper()
	saved, err := st.Put(context.Background(), doc, scope, "test")
	if err != nil {
		t.Fatalf("put agent def: %v", err)
	}
	return saved
}

// TestPGStorePutGetRoundTrip: a saved definition reads back at its scope with
// every parsed field and the raw document intact.
func TestPGStorePutGetRoundTrip(t *testing.T) {
	db := pgDefDB(t)
	st := NewPGStore(db)
	ctx := context.Background()

	name := "test-def-" + defSuffix()
	scope := identity.UserScope("user-" + defSuffix())
	saved := putDef(t, st, defDoc(name, "You are a test agent."), scope)
	cleanupDef(t, db, saved.ID)

	if saved.Version != 1 || saved.Name != name || saved.System != "You are a test agent." {
		t.Fatalf("saved def: %+v", saved)
	}
	if saved.RawDocument == "" || saved.WhenToUse == "" {
		t.Fatalf("raw document and when-to-use must be retained: %+v", saved)
	}
	if len(saved.Tools) != 1 || saved.Tools[0] != "read_file" || len(saved.Skills) != 1 {
		t.Fatalf("frontmatter lists decoded: %+v", saved)
	}

	got, err := st.Get(ctx, name, scope)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != saved.ID || got.System != saved.System {
		t.Fatalf("get round trip: %+v", got)
	}
}

// TestPGStoreVersionIncrements: each save appends a new immutable version and
// bumps the pointer; history is never rewritten.
func TestPGStoreVersionIncrements(t *testing.T) {
	db := pgDefDB(t)
	st := NewPGStore(db)

	name := "test-def-" + defSuffix()
	scope := identity.TeamScope("team-" + defSuffix())
	first := putDef(t, st, defDoc(name, "v1 body"), scope)
	cleanupDef(t, db, first.ID)
	second := putDef(t, st, defDoc(name, "v2 body"), scope)

	if second.Version != 2 || second.ID != first.ID || second.System != "v2 body" {
		t.Fatalf("version bump: first=%+v second=%+v", first, second)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM agent_def_versions WHERE def_id = $1`, first.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 immutable versions, got %d", n)
	}
}

// TestPGStoreListVisibleScopes: visibility union across scopes; a def at one
// scope is invisible at another owner.
func TestPGStoreListVisibleScopes(t *testing.T) {
	db := pgDefDB(t)
	st := NewPGStore(db)
	ctx := context.Background()

	userA := "user-" + defSuffix()
	userB := "user-" + defSuffix()
	name := "test-def-" + defSuffix()
	mine := putDef(t, st, defDoc(name, "body"), identity.UserScope(userA))
	cleanupDef(t, db, mine.ID)

	vis, err := st.ListVisible(ctx, []identity.ScopeRef{identity.UserScope(userA), identity.SystemScope()})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range vis {
		if d.ID == mine.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("own def must be visible, got %d defs", len(vis))
	}

	vis, err = st.ListVisible(ctx, []identity.ScopeRef{identity.UserScope(userB), identity.SystemScope()})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range vis {
		if d.ID == mine.ID {
			t.Fatalf("another user's def must not be visible")
		}
	}
}

// TestPGStoreDeleteIsolation: delete removes only the exact (name, scope)
// definition; a same-named def at another scope survives; deleting a missing
// name is ErrNotFound.
func TestPGStoreDeleteIsolation(t *testing.T) {
	db := pgDefDB(t)
	st := NewPGStore(db)
	ctx := context.Background()

	name := "test-def-" + defSuffix()
	userScope := identity.UserScope("user-" + defSuffix())
	sysScope := identity.SystemScope()
	mine := putDef(t, st, defDoc(name, "user body"), userScope)
	sys := putDef(t, st, defDoc(name, "system body"), sysScope)
	cleanupDef(t, db, mine.ID)
	cleanupDef(t, db, sys.ID)

	if err := st.Delete(ctx, name, userScope); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, name, userScope); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted def must be gone, err=%v", err)
	}
	if _, err := st.Get(ctx, name, sysScope); err != nil {
		t.Fatalf("same-named system def must survive: %v", err)
	}
	if err := st.Delete(ctx, name, userScope); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete must be ErrNotFound, got %v", err)
	}
}

// TestPGStorePutValidates: invalid documents are rejected and nothing is
// stored.
func TestPGStorePutValidates(t *testing.T) {
	db := pgDefDB(t)
	st := NewPGStore(db)
	scope := identity.UserScope("user-" + defSuffix())

	for _, doc := range []string{
		"no frontmatter at all",
		"---\ndescription: missing name\n---\nbody\n",
		"---\nname: x\ndescription: y\n---\n", // empty body
	} {
		if _, err := st.Put(context.Background(), doc, scope, "test"); err == nil {
			t.Fatalf("invalid doc must be rejected: %q", doc)
		}
	}
}
