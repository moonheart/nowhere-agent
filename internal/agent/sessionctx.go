package agent

import "context"

// sessionIDKey tags a run's context with the id of the session that owns the run.
type sessionIDKey struct{}

// ContextWithSessionID returns a run context carrying its owning session id. The
// run registry sets it so call-time policies (the permission middleware's
// per-session mode) and tools (the subagent spawn tool, which propagates the id
// to child loops) can resolve session-scoped inputs from the context rather than
// at middleware-registration time.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext returns the session id a run context carries, or "" when
// it has none (a run started outside the session registry, e.g. tests/dev).
func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}
