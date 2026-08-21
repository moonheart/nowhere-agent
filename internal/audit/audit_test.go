package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"nowhere-agent/internal/trustedproxy"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgTestDB points at the real dev Postgres (see repo convention). Tests scope
// themselves with a unique actor id and delete only the rows they created.
func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := getenvOr("AUDIT_PG_TEST_DSN", "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable")
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

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetTrustedProxiesForTest points the process-wide trusted-proxy set at cidrs
// for the duration of the test and restores the secure default afterwards.
func SetTrustedProxiesForTest(t *testing.T, cidrs []string) {
	t.Helper()
	trustedproxy.SetDefault(cidrs)
	t.Cleanup(func() { trustedproxy.SetDefault(nil) })
}

// cleanupActor deletes only the audit rows this test created, keyed by a unique
// actor id. Never an unscoped DELETE.
func cleanupActor(t *testing.T, db *sql.DB, actorID string) {
	t.Helper()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM audit_log WHERE actor_id = $1`, actorID)
	})
}

func TestLogAndListRoundTrip(t *testing.T) {
	db := pgTestDB(t)
	actorID := "aud-" + randHex()
	cleanupActor(t, db, actorID)
	log := NewLogger(db, nil)
	ctx := context.Background()

	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "203.0.113.7:5150"
	r.Header.Set("User-Agent", "test-agent/1.0")

	err := log.Log(ctx, Success(ActionAuthLogin).
		FromRequest(r).
		Actor(actorID, "aud@example.com").
		Target("user", actorID).
		Detail(map[string]any{"method": "password"}))
	if err != nil {
		t.Fatalf("Log: %v", err)
	}

	entries, total, err := log.List(ctx, Filter{Actor: actorID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(entries) != 1 {
		t.Fatalf("expected 1 entry, got total=%d len=%d", total, len(entries))
	}
	e := entries[0]
	if e.Action != string(ActionAuthLogin) || e.Outcome != string(OutcomeSuccess) {
		t.Errorf("action/outcome = %q/%q", e.Action, e.Outcome)
	}
	if e.ActorID != actorID || e.ActorEmail != "aud@example.com" {
		t.Errorf("actor = %q/%q", e.ActorID, e.ActorEmail)
	}
	if e.IP != "203.0.113.7" || e.UA != "test-agent/1.0" {
		t.Errorf("client = %q/%q", e.IP, e.UA)
	}
	if e.TargetType != "user" || e.TargetID != actorID {
		t.Errorf("target = %q/%q", e.TargetType, e.TargetID)
	}
	if string(e.Detail) == "" || string(e.Detail) == "null" {
		t.Errorf("detail empty: %s", e.Detail)
	}
}

func TestAnonymousEventHasNullActor(t *testing.T) {
	db := pgTestDB(t)
	log := NewLogger(db, nil)
	ctx := context.Background()
	// A failed login carries no actor; the row must still insert (NULL actor).
	email := "nobody-" + randHex() + "@example.com"
	if err := log.Log(ctx, Failure(ActionAuthLogin).Detail(map[string]any{"email": email})); err != nil {
		t.Fatalf("Log anonymous: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM audit_log WHERE action = $1 AND detail->>'email' = $2`, string(ActionAuthLogin), email)
	})
	var actorID sql.NullString
	err := db.QueryRow(`SELECT actor_id FROM audit_log WHERE action = $1 AND detail->>'email' = $2`,
		string(ActionAuthLogin), email).Scan(&actorID)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if actorID.Valid {
		t.Errorf("anonymous event should have NULL actor_id, got %q", actorID.String)
	}
}

func TestListFiltersAndPagination(t *testing.T) {
	db := pgTestDB(t)
	actorID := "aud-" + randHex()
	cleanupActor(t, db, actorID)
	log := NewLogger(db, nil)
	ctx := context.Background()

	// Three events: two logins, one admin action.
	for i := 0; i < 2; i++ {
		if err := log.Log(ctx, Success(ActionAuthLogin).Actor(actorID, "f@example.com")); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Log(ctx, Success(ActionAdminUserCreate).Actor(actorID, "f@example.com").Target("user", "x")); err != nil {
		t.Fatal(err)
	}

	// Filter by action.
	entries, total, err := log.List(ctx, Filter{Actor: actorID, Action: string(ActionAuthLogin)})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(entries) != 2 {
		t.Errorf("action filter: total=%d len=%d, want 2", total, len(entries))
	}

	// Newest-first ordering.
	all, _, err := log.List(ctx, Filter{Actor: actorID})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Action != string(ActionAdminUserCreate) {
		t.Errorf("ordering: first should be the admin action, got %+v", all)
	}

	// Pagination: limit 2 returns the two newest, offset 2 the oldest.
	page, total, err := log.List(ctx, Filter{Actor: actorID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(page) != 2 {
		t.Errorf("page1: total=%d len=%d, want 3/2", total, len(page))
	}
	page2, _, err := log.List(ctx, Filter{Actor: actorID, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Errorf("page2: len=%d, want 1", len(page2))
	}
}

// ClientIP honours XFF only for peers inside the configured trusted-proxy set.
// With the secure default (no trusted proxy) a forged header is ignored.
func TestClientIPPrefersForwardedFor(t *testing.T) {
	SetTrustedProxiesForTest(t, []string{"10.0.0.0/8"})

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:9999"
	r.Header.Set("X-Forwarded-For", "198.51.100.23, 10.0.0.1")
	if got := ClientIP(r); got != "198.51.100.23" {
		t.Errorf("ClientIP with XFF = %q, want leftmost hop", got)
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "10.0.0.1:9999"
	if got := ClientIP(r2); got != "10.0.0.1" {
		t.Errorf("ClientIP without XFF = %q, want peer host", got)
	}
}

func TestClientIPIgnoresProxyHeadersByDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:9999"
	r.Header.Set("X-Forwarded-For", "198.51.100.23, 10.0.0.1")
	if got := ClientIP(r); got != "10.0.0.1" {
		t.Errorf("untrusted proxy must be ignored, got %q", got)
	}
}

func TestLogAndReportSwallowsError(t *testing.T) {
	// A closed DB makes Log fail; LogAndReport must not panic or surface it.
	db := pgTestDB(t)
	db.Close()
	log := NewLogger(db, nil)
	log.LogAndReport(context.Background(), Success(ActionAuthLogin).Actor("x", "x@e.com")) // must not panic
}

// TestPurgeBefore: the retention sweep deletes rows by age only — rows older
// than the cutoff go, newer rows stay. The test's "old" rows are backdated to
// 2001 so the cutoff (2002) can never touch another test's (or a real
// deployment's) recent rows — the age window itself is the scope.
func TestPurgeBefore(t *testing.T) {
	db := pgTestDB(t)
	actorID := "aud-purge-" + randHex()
	cleanupActor(t, db, actorID)
	log := NewLogger(db, nil)
	ctx := context.Background()

	// Two rows backdated into 2001 (inside the purge window) and one live row
	// (outside it).
	for range 2 {
		if _, err := db.Exec(`
			INSERT INTO audit_log (created_at, actor_id, action, outcome)
			VALUES ('2001-06-01T00:00:00Z', $1, 'auth.login', 'success')`, actorID); err != nil {
			t.Fatalf("backdate insert: %v", err)
		}
	}
	if err := log.Log(ctx, Success(ActionAuthLogin).Actor(actorID, "p@example.com")); err != nil {
		t.Fatalf("live insert: %v", err)
	}

	removed, err := log.PurgeBefore(ctx, time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want exactly the 2 backdated rows", removed)
	}

	entries, total, err := log.List(ctx, Filter{Actor: actorID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(entries) != 1 {
		t.Fatalf("after purge: total=%d len=%d, want only the live row", total, len(entries))
	}
}
