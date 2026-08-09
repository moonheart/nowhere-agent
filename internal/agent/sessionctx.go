package agent

import (
	"context"

	"nowhere-agent/internal/reqctx"
)

// ContextWithSessionID returns a run context carrying its owning session id. The
// run registry sets it so call-time policies (the permission middleware's
// per-session mode) and tools (the subagent spawn tool, which propagates the id
// to child loops) can resolve session-scoped inputs from the context rather than
// at middleware-registration time. The value lives in reqctx, the shared typed
// home for request-scoped values.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return reqctx.WithSessionID(ctx, sessionID)
}

// SessionIDFromContext returns the session id a run context carries, or "" when
// it has none (a run started outside the session registry, e.g. tests/dev).
func SessionIDFromContext(ctx context.Context) string {
	return reqctx.SessionID(ctx)
}
