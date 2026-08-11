package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3_000_000_000)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRuntimeDefaultsAndOverrides(t *testing.T) {
	store := NewStore(testDB(t))
	ctx := context.Background()
	key := "test_http_allowlist"
	t.Cleanup(func() { store.db.Exec(`DELETE FROM platform_settings WHERE key = $1`, key) })

	rt := NewRuntime(store, map[string]json.RawMessage{
		key: raw(t, "api.example.com"),
	}, slog.Default())
	if got := rt.String(key); got != "api.example.com" {
		t.Fatalf("default not applied: %q", got)
	}

	// Override via Set: value changes immediately, no reload needed.
	if err := rt.Set(ctx, key, raw(t, "*.internal")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := rt.String(key); got != "*.internal" {
		t.Fatalf("override not applied: %q", got)
	}

	// A fresh runtime loads the persisted row (boot semantics).
	rt2 := NewRuntime(store, map[string]json.RawMessage{key: raw(t, "default")}, slog.Default())
	if err := rt2.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := rt2.String(key); got != "*.internal" {
		t.Fatalf("load did not pick up persisted row: %q", got)
	}

	// Setting null removes the row → back to the default.
	if err := rt.Set(ctx, key, json.RawMessage("null")); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := rt.String(key); got != "api.example.com" {
		t.Fatalf("cleared setting did not fall back to default: %q", got)
	}
}

func TestRuntimeTypedAccess(t *testing.T) {
	store := NewStore(testDB(t))
	rt := NewRuntime(store, map[string]json.RawMessage{
		"rate_limit_rps":   raw(t, 20),
		"llm_system_lang":  raw(t, "zh"),
		"missing":          raw(t, "ignored"),
	}, slog.Default())
	defer func() {
		store.db.Exec(`DELETE FROM platform_settings WHERE key IN ('rate_limit_rps','llm_system_lang')`)
	}()

	if got := rt.Int("rate_limit_rps"); got != 20 {
		t.Fatalf("int = %d, want 20", got)
	}
	if got := rt.String("llm_system_lang"); got != "zh" {
		t.Fatalf("lang = %q, want zh", got)
	}
	// Unknown key with no default → zero values.
	if got := rt.String("nope"); got != "" {
		t.Fatalf("unknown string = %q", got)
	}
	if got := rt.Int("nope"); got != 0 {
		t.Fatalf("unknown int = %d", got)
	}
}
