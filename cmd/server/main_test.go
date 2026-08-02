package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/config"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/routing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() config.Config {
	var cfg config.Config
	cfg.LLM.Provider = "anthropic"
	cfg.LLM.APIKey = "platform-key"
	return cfg
}

// stubAdapter stands in for the boot-time platform adapter, so a test can tell
// "fell back" from "built a new one" by identity.
type stubAdapter struct{ name string }

func (s stubAdapter) Name() string { return s.name }
func (s stubAdapter) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event)
	close(ch)
	return ch, nil
}

// A keystore that cannot be reached must not take chat down with it.
func TestAdapterForCallerFallsBackWhenResolutionFails(t *testing.T) {
	platform := stubAdapter{name: "platform"}
	// A closed pool makes every query fail, which is what a Postgres outage
	// looks like from here.
	db, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	keys := routing.NewPGKeyStore(db, "platform-key")

	ctx := identity.NewContextWithUser(context.Background(), identity.User{ID: "u1"})
	got := adapterForCaller(ctx, testConfig(), provider.NewRawRecorder(""), keys, platform, quietLogger())

	if got != provider.Adapter(platform) {
		t.Errorf("adapter = %v, want the platform adapter as fallback", got)
	}
}

// A request with no authenticated user (there is nobody to resolve a team key
// for) uses the platform adapter rather than erroring.
func TestAdapterForCallerWithoutUser(t *testing.T) {
	platform := stubAdapter{name: "platform"}
	db, _ := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	db.Close()

	got := adapterForCaller(context.Background(), testConfig(), provider.NewRawRecorder(""),
		routing.NewPGKeyStore(db, "platform-key"), platform, quietLogger())
	if got != provider.Adapter(platform) {
		t.Errorf("adapter = %v, want the platform adapter", got)
	}
}

func TestAdapterForCallerWithoutKeyStore(t *testing.T) {
	platform := stubAdapter{name: "platform"}
	ctx := identity.NewContextWithUser(context.Background(), identity.User{ID: "u1"})
	got := adapterForCaller(ctx, testConfig(), provider.NewRawRecorder(""), nil, platform, quietLogger())
	if got != provider.Adapter(platform) {
		t.Errorf("adapter = %v, want the platform adapter", got)
	}
}

// With a real database and a team key configured for the provider being called,
// the request gets an adapter bound to that key — not the platform one.
func TestAdapterForCallerUsesTeamKey(t *testing.T) {
	db := pgTestDB(t)
	platform := stubAdapter{name: "platform"}

	var userID, teamID string
	err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`,
		"srv-"+randSuffix()+"@test.dev").Scan(&userID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	if err := db.QueryRow(`INSERT INTO teams (name) VALUES ($1) RETURNING id`, "srv-"+randSuffix()).Scan(&teamID); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM teams WHERE id = $1`, teamID) })
	if _, err := db.Exec(`INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1,$2,'owner')`, teamID, userID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO team_api_keys (team_id, provider, api_key) VALUES ($1,'anthropic','team-secret')`, teamID); err != nil {
		t.Fatalf("add key: %v", err)
	}

	keys := routing.NewPGKeyStore(db, "platform-key")
	ctx := identity.NewContextWithUser(context.Background(), identity.User{ID: userID})
	got := adapterForCaller(ctx, testConfig(), provider.NewRawRecorder(""), keys, platform, quietLogger())

	if got == provider.Adapter(platform) {
		t.Fatal("a configured team key was ignored; the platform adapter was used")
	}
	if got.Name() != "anthropic" {
		t.Errorf("adapter name = %q, want anthropic", got.Name())
	}
}

// A team with no key for the provider being called falls through to the
// platform adapter.
func TestAdapterForCallerWithoutTeamKey(t *testing.T) {
	db := pgTestDB(t)
	platform := stubAdapter{name: "platform"}

	var userID string
	if err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`,
		"srv2-"+randSuffix()+"@test.dev").Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	keys := routing.NewPGKeyStore(db, "platform-key")
	ctx := identity.NewContextWithUser(context.Background(), identity.User{ID: userID})
	got := adapterForCaller(ctx, testConfig(), provider.NewRawRecorder(""), keys, platform, quietLogger())

	if got != provider.Adapter(platform) {
		t.Errorf("adapter = %v, want the platform adapter when no team key applies", got)
	}
}

// ---- SPA fallback ----

func TestSPAHandlerServesRealFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), "<!doctype html><title>shell</title>")
	write(t, filepath.Join(dir, "app.js"), "console.log(1)")

	rec := serve(t, spaHandler(dir), "/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("asset body = %q, want the file's contents", rec.Body.String())
	}
}

// The point of the fallback: a client-side route has no file behind it, and a
// plain FileServer would 404 a shared link or a reload.
func TestSPAHandlerFallsBackForClientRoutes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), "<!doctype html><title>shell</title>")

	for _, p := range []string{"/admin", "/admin/users", "/admin/teams/abc/members"} {
		rec := serve(t, spaHandler(dir), p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", p, rec.Code)
		}
		if rec.Body.String() != "<!doctype html><title>shell</title>" {
			t.Errorf("%s served %q, want the app shell", p, rec.Body.String())
		}
	}
}

func TestSPAHandlerServesIndexAtRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), "shell")

	rec := serve(t, spaHandler(dir), "/")
	if rec.Code != http.StatusOK || rec.Body.String() != "shell" {
		t.Errorf("/ = %d %q, want 200 with the shell", rec.Code, rec.Body.String())
	}
}

// The fallback must not shadow the API: those patterns are more specific, and
// Go's ServeMux prefers them. Registering both and asking for an API path
// proves the precedence rather than assuming it.
func TestSPAFallbackDoesNotShadowAPIRoutes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), "shell")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user":{}}`))
	})
	mux.Handle("GET /", spaHandler(dir))

	rec := serve(t, mux, "/api/me")
	if rec.Body.String() == "shell" {
		t.Error("the SPA fallback swallowed an API route")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("/api/me = %d, want 200 from the API handler", rec.Code)
	}

	// An API path with no handler still reaches the fallback rather than
	// 404-ing, which is acceptable — but a registered one must not.
	rec = serve(t, mux, "/admin/users")
	if rec.Body.String() != "shell" {
		t.Errorf("/admin/users = %q, want the app shell", rec.Body.String())
	}
}

func TestSPAHandlerRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), "shell")
	// A file the handler must never serve, one level above the web root.
	outside := filepath.Join(filepath.Dir(dir), "secret-"+randSuffix()+".txt")
	write(t, outside, "top secret")
	t.Cleanup(func() { os.Remove(outside) })

	rec := serve(t, spaHandler(dir), "/../"+filepath.Base(outside))
	if rec.Body.String() == "top secret" {
		t.Fatal("path traversal served a file outside the web root")
	}
}

func serve(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
	}
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

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
