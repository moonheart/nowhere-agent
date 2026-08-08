package quota

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterDisabledPassesEverything(t *testing.T) {
	rl := NewRateLimiter(0, 0, nil)
	for i := 0; i < 100; i++ {
		if !rl.Allow("k") {
			t.Fatal("disabled limiter must allow everything")
		}
	}
}

func TestRateLimiterBurstThenThrottle(t *testing.T) {
	// rps so low that after the burst is spent, tokens do not refill meaningfully
	// within the test; burst=3 admits exactly 3 then blocks.
	rl := NewRateLimiter(0.0001, 3, nil)
	for i := 0; i < 3; i++ {
		if !rl.Allow("k") {
			t.Fatalf("request %d within burst should be allowed", i+1)
		}
	}
	if rl.Allow("k") {
		t.Fatal("burst exhausted should block")
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	rl := NewRateLimiter(0.0001, 1, nil)
	if !rl.Allow("a") {
		t.Fatal("a first request should pass")
	}
	if !rl.Allow("b") {
		t.Fatal("b has its own bucket and should pass")
	}
	if rl.Allow("a") {
		t.Fatal("a's bucket is spent")
	}
	if rl.Allow("b") {
		t.Fatal("b's bucket is spent")
	}
}

func TestRateLimiterMiddlewareRejectsOverLimit(t *testing.T) {
	rl := NewRateLimiter(0.0001, 1, func(*http.Request) string { return "fixed" })
	var served int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++; w.WriteHeader(http.StatusOK) })
	h := rl.Middleware(next)

	// First request passes.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK || served != 1 {
		t.Fatalf("first request should pass: code=%d served=%d", rec.Code, served)
	}
	// Second is rejected with 429 and a Retry-After hint.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit should be 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 should carry a Retry-After hint")
	}
	if served != 1 {
		t.Fatal("rejected request must not reach the next handler")
	}
}

func TestRateLimiterMiddlewareEmptyKeyBypasses(t *testing.T) {
	rl := NewRateLimiter(0.0001, 1, func(*http.Request) string { return "" })
	var served int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++ })
	h := rl.Middleware(next)
	for i := 0; i < 5; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	if served != 5 {
		t.Fatalf("empty key (opt-out) should bypass limiting, served=%d", served)
	}
}

func TestRateLimiterMiddlewareDisabledPassesThrough(t *testing.T) {
	rl := NewRateLimiter(0, 0, func(*http.Request) string { return "k" })
	var served int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++ })
	h := rl.Middleware(next)
	for i := 0; i < 50; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}
	if served != 50 {
		t.Fatalf("disabled limiter must pass all requests, served=%d", served)
	}
}

func TestClientIPKeyPrefersForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 70.41.3.18")
	if got := ClientIPKey(r); got != "203.0.113.7" {
		t.Fatalf("should take the first X-Forwarded-For hop, got %q", got)
	}
}

func TestClientIPKeyStripsPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:9999"
	if got := ClientIPKey(r); got != "192.0.2.1" {
		t.Fatalf("should strip the source port, got %q", got)
	}
}

func TestClientIPKeySharesAcrossSourcePorts(t *testing.T) {
	a := httptest.NewRequest(http.MethodGet, "/", nil)
	a.RemoteAddr = "192.0.2.1:1111"
	b := httptest.NewRequest(http.MethodGet, "/", nil)
	b.RemoteAddr = "192.0.2.1:2222"
	if ClientIPKey(a) != ClientIPKey(b) {
		t.Fatal("one client across many source ports should share a bucket")
	}
}
