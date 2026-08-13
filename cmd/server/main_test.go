package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/permission"
	"nowhere-agent/internal/trustedproxy"
)

// ---- SPA fallback ----

// TestRateLimitKeyKeysAuthenticatedBySession pins the limiter's key choice:
// a bearer token keys by the session (not the IP, so NAT-mates never share a
// bucket), anonymous stays on the client IP, probes opt out, and a malformed
// bearer falls back to the IP key (anonymous flood smoothing unchanged).
func TestRateLimitKey(t *testing.T) {
	proxies := trustedproxy.New(nil)

	for _, p := range []string{"/healthz", "/metrics"} {
		req := httptest.NewRequest("GET", p, nil)
		if got := rateLimitKey(req, proxies); got != "" {
			t.Errorf("rateLimitKey(%s) = %q, want \"\" (probe opt-out)", p, got)
		}
	}

	anon := httptest.NewRequest("GET", "/api/chat", nil)
	ip := anon.RemoteAddr
	if host, _, err := net.SplitHostPort(anon.RemoteAddr); err == nil {
		ip = host
	}
	if got := rateLimitKey(anon, proxies); got != ip {
		t.Errorf("anonymous key = %q, want client IP %q", got, ip)
	}

	auth := httptest.NewRequest("GET", "/api/chat", nil)
	auth.Header.Set("Authorization", "Bearer abc123")
	got := rateLimitKey(auth, proxies)
	if !strings.HasPrefix(got, "auth:") || len(got) != len("auth:")+64 {
		t.Errorf("bearer key = %q, want \"auth:\" + sha256 hex", got)
	}
	other := httptest.NewRequest("GET", "/api/chat", nil)
	other.Header.Set("Authorization", "Bearer xyz789")
	if got == rateLimitKey(other, proxies) {
		t.Error("different sessions must map to different buckets")
	}

	malformed := httptest.NewRequest("GET", "/api/chat", nil)
	malformed.Header.Set("Authorization", "Bearer ")
	if got := rateLimitKey(malformed, proxies); got != ip {
		t.Errorf("empty bearer key = %q, want fallback to client IP %q", got, ip)
	}
	basic := httptest.NewRequest("GET", "/api/chat", nil)
	basic.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := rateLimitKey(basic, proxies); got != ip {
		t.Errorf("non-bearer auth key = %q, want fallback to client IP %q", got, ip)
	}
}

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

// A real directory under the web root must never be served as a FileServer
// listing: it falls back to the app shell like any client route.
func TestSPAHandlerDoesNotListDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "index.html"), "shell")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "assets", "app.js"), "console.log(1)")

	for _, p := range []string{"/assets", "/assets/"} {
		rec := serve(t, spaHandler(dir), p)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", p, rec.Code)
		}
		body := rec.Body.String()
		if body == "console.log(1)" || strings.Contains(body, "app.js") || strings.Contains(body, "Index of") {
			t.Errorf("%s served a directory listing: %q", p, body)
		}
		if body != "shell" {
			t.Errorf("%s = %q, want the app shell", p, body)
		}
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

// TestClampPermissionDecision pins the fail-closed clamp: a known value passes
// through, and anything else (corrupt setting, manual DB edit) becomes deny so
// neither execution gate can silently open.
// TestNewWebhookGuardEmptyListIsStrict pins the fail-closed wiring: the
// default (empty) allowlist must still produce a guard — not nil — and that
// guard must refuse private targets while passing public ones. The guard is
// the only thing screening user-written webhook URLs, so an empty allowlist
// must never silently disable it.
func TestNewWebhookGuardEmptyListIsStrict(t *testing.T) {
	g, err := newWebhookGuard(nil)
	if err != nil {
		t.Fatalf("empty allowlist must build a guard, got error: %v", err)
	}
	if g == nil {
		t.Fatal("empty allowlist built a nil guard")
	}
	// Literal-IP targets are decided without DNS, keeping the test hermetic.
	for _, u := range []string{"http://127.0.0.1:1/x", "http://10.0.0.1/x", "http://[::1]/x", "http://169.254.169.254/x"} {
		if err := g.CheckURL(context.Background(), u); err == nil {
			t.Errorf("CheckURL(%q): accepted, want block under the empty allowlist", u)
		}
	}
	if err := g.CheckURL(context.Background(), "http://8.8.8.8/x"); err != nil {
		t.Errorf("CheckURL(public): %v, want ok under the empty allowlist", err)
	}
	if _, err := newWebhookGuard([]string{"not-a-cidr"}); err == nil {
		t.Error("malformed allowlist CIDR must be an error, not a silent skip")
	}
	if _, err := newWebhookGuard([]string{"10.0.0.0/8", "172.16.0.0/12"}); err != nil {
		t.Errorf("well-formed allowlist must build a guard: %v", err)
	}
}

func TestClampPermissionDecision(t *testing.T) {
	for _, v := range []string{"allow", "ask", "deny"} {
		if got := clampPermissionDecision(v, "k"); got != permission.Decision(v) {
			t.Errorf("clamp(%q) = %q, want pass-through %q", v, got, v)
		}
	}
	for _, v := range []string{"", "ALLOW", "maybe", "1"} {
		if got := clampPermissionDecision(v, "k"); got != permission.DecisionDeny {
			t.Errorf("clamp(%q) = %q, want deny (fail-closed)", v, got)
		}
	}
}
