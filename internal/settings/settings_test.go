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

func TestRuntimeBoolFloatDuration(t *testing.T) {
	store := NewStore(testDB(t))
	ctx := context.Background()
	rt := NewRuntime(store, map[string]json.RawMessage{
		"dreaming_enabled":       raw(t, true),
		"llm_temperature":        raw(t, 0.7),
		"llm_stream_idle_timeout": raw(t, 120),
	}, slog.Default())
	defer func() {
		store.db.Exec(`DELETE FROM platform_settings WHERE key IN ('dreaming_enabled','llm_temperature','llm_stream_idle_timeout')`)
	}()

	if !rt.Bool("dreaming_enabled") {
		t.Fatal("bool = false, want true")
	}
	if rt.Bool("nope") {
		t.Fatal("unknown bool = true, want false")
	}
	if got := rt.Float64("llm_temperature"); got != 0.7 {
		t.Fatalf("float = %v, want 0.7", got)
	}
	if got := rt.Duration("llm_stream_idle_timeout"); got.Seconds() != 120 {
		t.Fatalf("duration = %v, want 120s", got)
	}
	if rt.Duration("nope") != 0 {
		t.Fatal("unknown duration != 0")
	}

	// Set + reload round trip for a float override.
	if err := rt.Set(ctx, "llm_temperature", raw(t, -1.0)); err != nil {
		t.Fatalf("set float: %v", err)
	}
	if got := rt.Float64("llm_temperature"); got != -1.0 {
		t.Fatalf("float override = %v", got)
	}
	rt2 := NewRuntime(store, map[string]json.RawMessage{"llm_temperature": raw(t, 0.5)}, slog.Default())
	if err := rt2.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := rt2.Float64("llm_temperature"); got != -1.0 {
		t.Fatalf("loaded float = %v, want -1", got)
	}
}

func TestCatalogCoversAllGroupsAndKinds(t *testing.T) {
	infos := Catalog()
	if len(infos) < 30 {
		t.Fatalf("catalog has %d entries, want >= 30", len(infos))
	}
	seen := map[string]bool{}
	for _, i := range infos {
		if seen[i.Key] {
			t.Fatalf("duplicate key %q in catalog", i.Key)
		}
		seen[i.Key] = true
		if i.Group == "" || i.Kind == "" || i.Description == "" {
			t.Fatalf("incomplete entry %q (group=%q kind=%q)", i.Key, i.Group, i.Kind)
		}
	}
	for _, g := range []Group{GroupTools, GroupWebhooks, GroupLLM, GroupSandbox, GroupPermissions, GroupRedaction, GroupSubagents, GroupBackground, GroupHTTP} {
		found := false
		for _, i := range infos {
			if i.Group == g {
				found = true
			}
		}
		if !found {
			t.Fatalf("group %q has no entries", g)
		}
	}
	// The rate-limit keys must exist and be typed.
	if Info("rate_limit_rps").Kind != KindFloat {
		t.Fatalf("rate_limit_rps kind = %q, want float", Info("rate_limit_rps").Kind)
	}
	if Info("rate_limit_burst").Kind != KindInt {
		t.Fatalf("rate_limit_burst kind = %q, want int", Info("rate_limit_burst").Kind)
	}
	if !Info(KeyWebhookSigningSecret).Secret {
		t.Fatal("webhook_signing_secret must be marked secret")
	}
	if Info("bogus").Key != "" {
		t.Fatalf("unknown key resolved to %q", Info("bogus").Key)
	}
}
