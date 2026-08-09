package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRouterMountAppliesMiddlewareOnce pins the Router contract that is the
// whole point of the type: the middleware set wraps the group exactly once at
// Mount, and every route in the group goes through it.
func TestRouterMountAppliesMiddlewareOnce(t *testing.T) {
	var hits int
	outer := http.NewServeMux()
	g := NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			next.ServeHTTP(w, r)
		})
	})
	g.HandleFunc("GET /api/a", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	g.HandleFunc("GET /api/b", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	g.Mount(outer, "/api/")

	rec := httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest("GET", "/api/a", nil))
	if rec.Code != 200 || hits != 1 {
		t.Fatalf("first route: code=%d hits=%d, want 200 and one middleware pass", rec.Code, hits)
	}
	rec = httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest("GET", "/api/b", nil))
	if hits != 2 {
		t.Fatalf("second route: hits=%d, want the middleware applied once per request", hits)
	}
}

// TestRouterMountOpenRouteBeatsSubtree pins the load-bearing precedence rule
// that lets open routes (auth, oidc, healthz) live on the outer mux while the
// protected tier hangs off a subtree: a more specific pattern wins, so an open
// "POST /api/auth/signup" does NOT pass through the protected group's auth.
func TestRouterMountOpenRouteBeatsSubtree(t *testing.T) {
	outer := http.NewServeMux()

	// Open route registered on the outer mux directly.
	outer.HandleFunc("POST /api/auth/signup", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	// Protected tier whose auth middleware would reject everything.
	denied := false
	g := NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			denied = true
			http.Error(w, "auth required", http.StatusUnauthorized)
		})
	})
	g.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	g.Mount(outer, "/api/")

	rec := httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth/signup", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("open route shadowed by the protected subtree: code=%d, want 201", rec.Code)
	}
	if denied {
		t.Fatal("open route reached the protected group's middleware")
	}

	rec = httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))
	if rec.Code != http.StatusUnauthorized || !denied {
		t.Fatalf("protected route: code=%d denied=%v, want 401 and the middleware to run", rec.Code, denied)
	}
}

// TestStatusForPinsSentinelTranslation pins the one status-mapping boundary:
// a plain error is 500, a statusCarrier is honored, and a wrapped one is found
// through errors.As.
func TestStatusForPinsSentinelTranslation(t *testing.T) {
	if got := StatusFor(errors.New("boom")); got != http.StatusInternalServerError {
		t.Errorf("plain error = %d, want 500", got)
	}
	if got := StatusFor(&typedStatus{status: http.StatusConflict}); got != http.StatusConflict {
		t.Errorf("carrier = %d, want 409", got)
	}
	if got := StatusFor(&wrapped{err: &typedStatus{status: http.StatusNotFound}}); got != http.StatusNotFound {
		t.Errorf("wrapped carrier = %d, want 404", got)
	}
}

// TestErrorFromWritesShapeOnly checks that the error surface leaks the status
// text — never the underlying error message.
func TestErrorFromWritesShapeOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	ErrorFrom(rec, errors.New("secret: postgres password is hunter2"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"error\":\"Internal Server Error\"}\n" {
		t.Errorf("body = %q, want the status text shape", got)
	}
}

type typedStatus struct{ status int }

func (t *typedStatus) Error() string   { return http.StatusText(t.status) }
func (t *typedStatus) HTTPStatus() int { return t.status }

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
