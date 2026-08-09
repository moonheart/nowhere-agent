package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

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
