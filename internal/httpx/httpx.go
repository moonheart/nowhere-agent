// Package httpx is the platform's shared HTTP plumbing, kept free of any
// internal dependency so every package can use it without a cycle. It provides
//
//   - the one JSON error surface (Error) and the domain-sentinel → HTTP-status
//     translation layer (StatusFor), so handlers stop hand-writing {"error": …}
//     bodies and a "known error" is normalized at one boundary instead of being
//     invented per handler;
//   - Router, the "protected/open route tier" primitive: a group of routes is
//     wrapped by one middleware set once, at Mount time, instead of each route
//     wrapping itself in auth() at registration — the pattern langgraph_api's
//     Mount(..., middleware=[auth, encryption]) uses.
//
// Middleware is an alias for func(http.Handler) http.Handler, so any package's
// middleware (observability, quota, identity, …) composes here without
// conversions.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Middleware is the standard http middleware shape.
type Middleware = func(http.Handler) http.Handler

// ErrBodyTooLarge is returned by ReadBodyMax when the request body exceeds the
// bound. Callers map it to HTTP 413 (request entity too large) before decoding.
var ErrBodyTooLarge = errors.New("request body too large")

// ReadBodyMax reads the request body bounded at maxBytes. A body beyond the
// bound must surface as "too large" (ErrBodyTooLarge), never as truncated
// invalid JSON — callers decode the returned bytes after checking the error.
// The LimitReader reads at most maxBytes+1, so an oversized body is detected
// without buffering the whole request.
func ReadBodyMax(r *http.Request, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, ErrBodyTooLarge
	}
	return body, nil
}

// DecodeBody reads a JSON request body bounded at maxBytes into v, answering
// the standard error responses itself: 413 for an oversized body, 400 for a
// read failure or malformed JSON. It reports false when the response has
// already been written. This is the shared implementation of the per-package
// `decode` helpers, so error wording cannot drift between handlers.
func DecodeBody(w http.ResponseWriter, r *http.Request, maxBytes int64, v any) bool {
	body, err := ReadBodyMax(r, maxBytes)
	if err != nil {
		if errors.Is(err, ErrBodyTooLarge) {
			Error(w, http.StatusRequestEntityTooLarge, "payload too large")
			return false
		}
		Error(w, http.StatusBadRequest, "read body")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		Error(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

// JSON writes v as a JSON response with the given status. It is the single
// wire-format home for API bodies; per-package writeJSON helpers delegate here
// so the shape cannot drift between handlers.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes the standard JSON error body {"error": msg} with the given
// status. Every handler — and the Recovery middleware — answers through this,
// so clients see one consistent error shape across the whole API.
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}

// statusCarrier is implemented by domain errors that know the HTTP status they
// map to. The platform's auth sentinels (identity package) carry their status
// this way; any future sentinel can do the same.
type statusCarrier interface {
	HTTPStatus() int
}

// StatusFor maps a panic value or error to an HTTP status. A value whose error
// (or any wrapped error) implements HTTPStatus reports that status; everything
// else falls back to 500. This is what lets the panic/known-error boundary
// (Recovery) answer 401/409/… for typed domain errors instead of a blanket 500.
func StatusFor(v any) int {
	if sc, ok := v.(statusCarrier); ok {
		return sc.HTTPStatus()
	}
	if e, ok := v.(error); ok {
		var sc statusCarrier
		if errors.As(e, &sc) {
			return sc.HTTPStatus()
		}
	}
	return http.StatusInternalServerError
}

// ErrorFrom writes the standard JSON error body for err, mapping its status via
// StatusFor. It never leaks the error's message — the body is the status text,
// which is safe to show clients while the real detail stays in the logs.
func ErrorFrom(w http.ResponseWriter, err error) {
	status := StatusFor(err)
	msg := http.StatusText(status)
	if msg == "" {
		msg = "request failed"
	}
	Error(w, status, msg)
}

// Router groups a set of routes behind a shared middleware set. Routes
// register onto the Router's own mux with their normal absolute patterns
// ("POST /api/chat", …); Mount attaches the whole group to an outer mux under a
// prefix ("/api/"), applying the middleware set exactly ONCE to every route in
// the group (first middleware outermost). This is the "protected/open route
// tier": main.go declares the tier — auth here, CSRF/encryption-context/tenant
// later — and the handlers just register routes into it, so adding a per-route
// concern never touches a Register method again.
type Router struct {
	mux *http.ServeMux
	mws []Middleware
}

// NewRouter builds an empty route group. mws are applied (first = outermost) to
// every route registered on the group when it is Mount-ed. With no mws the
// group is an open tier.
func NewRouter(mws ...Middleware) *Router {
	return &Router{mux: http.NewServeMux(), mws: mws}
}

// Handle registers one pattern on the group.
func (r *Router) Handle(pattern string, h http.Handler) {
	r.mux.Handle(pattern, h)
}

// HandleFunc registers one pattern on the group.
func (r *Router) HandleFunc(pattern string, h http.HandlerFunc) {
	r.mux.HandleFunc(pattern, h)
}

// Mount registers the group's chained mux onto outer at prefix (typically
// "/api/"). Route patterns inside the group stay absolute; ServeMux passes the
// full path to the subtree mux, so matching works unchanged. A more specific
// pattern already registered on outer (e.g. open auth routes like
// "POST /api/auth/signup") wins over the subtree, keeping open routes open.
func (r *Router) Mount(outer *http.ServeMux, prefix string) {
	h := http.Handler(r.mux)
	for i := len(r.mws) - 1; i >= 0; i-- {
		h = r.mws[i](h)
	}
	outer.Handle(prefix, h)
}
