// Package reqctx is the single home for the request-scoped values the platform
// threads through contexts: the correlation request id, the request-scoped
// slog logger, the authenticated user, and the owning session id.
//
// Before this package those keys were private context-key values scattered
// across four packages (observability, identity, agent) and threaded between
// them by context-value osmosis. Centralizing them here makes three things
// explicit:
//
//   - one key scheme, so a value written by the HTTP layer is read by the agent
//     layer without each side guessing the other's private key;
//   - a typed handoff boundary (Detach) for background workers (run
//     goroutines) that must keep the caller's correlation values without its
//     cancellation;
//   - the old per-package accessors (observability.FromContext, identity.
//     UserFromContext, agent.SessionIDFromContext, …) all become thin wrappers,
//     so existing call sites are unchanged.
//
// The package imports nothing internal, so any package may depend on it.
package reqctx

import (
	"context"
	"log/slog"
)

// ctxKey is the unexported key type, so only this package can collide with it.
type ctxKey struct{ name string }

func (k ctxKey) String() string { return "reqctx." + k.name }

var (
	requestIDKey = ctxKey{"request-id"}
	loggerKey    = ctxKey{"logger"}
	userKey      = ctxKey{"user"}
	sessionIDKey = ctxKey{"session-id"}
)

// WithRequestID returns a context carrying id (the correlation id for one
// request or background task). An empty id is stored as-is; RequestID returns
// "" for a context that never carried one.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the context's correlation id, or "" when none is present.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithLogger returns a context carrying the scoped logger.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, log)
}

// Logger returns the context's scoped logger, or nil when none is present.
// Callers that need to log unconditionally fall back to slog.Default().
func Logger(ctx context.Context) *slog.Logger {
	log, _ := ctx.Value(loggerKey).(*slog.Logger)
	return log
}

// WithUser returns a context carrying the authenticated principal. The value is
// stored as any; identity re-stamps its typed User on the way out, so the
// domain package owns the concrete type.
func WithUser(ctx context.Context, u any) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// User returns the context's authenticated principal. The boolean reports
// presence; a context that never passed through auth yields (nil, false).
func User(ctx context.Context) (any, bool) {
	v := ctx.Value(userKey)
	return v, v != nil
}

// WithSessionID returns a context carrying the id of the session that owns the
// current run.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionIDKey, id)
}

// SessionID returns the context's owning session id, or "" when none is
// present (a run started outside the session registry, e.g. tests/dev).
func SessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey).(string)
	return id
}

// Detach derives a context for background work — a run goroutine spawned by an
// HTTP request — that is decoupled from the caller's cancellation (the run must
// outlive the submitting connection) but explicitly carries the caller's
// request-scoped values: request id, logger, user, and session id. It is the
// typed handoff boundary: background workers get their correlation values via
// the reqctx accessors rather than relying on context-value osmosis through
// WithoutCancel, so the contract is pinned in one place and survives any future
// key-scheme refactor.
func Detach(orig context.Context) context.Context {
	ctx := context.WithoutCancel(orig)
	ctx = WithRequestID(ctx, RequestID(orig))
	if log := Logger(orig); log != nil {
		ctx = WithLogger(ctx, log)
	}
	if u, ok := User(orig); ok {
		ctx = WithUser(ctx, u)
	}
	ctx = WithSessionID(ctx, SessionID(orig))
	return ctx
}
