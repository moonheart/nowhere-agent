package observability

import (
	"log/slog"
	"net/http"
)

// StandardStack composes the inbound HTTP middleware stack around h, outermost
// first. It is the single named assembly point for the platform stack (the
// HTTP analogue of the agent loop's useStandardMiddleware), so the order
// invariants live in one place instead of being re-nested by hand at every
// change. Order invariants:
//
//   - request-id outermost: an id is near-free (one random read), and giving
//     EVERY request one — including requests the limiter is about to reject —
//     keeps throttled floods traceable instead of anonymous.
//   - access-log next: one line per completed request (status/latency/ttfb/
//     bytes, 499 on client disconnect), so rejected requests are also seen.
//   - rate-limit before metrics: a flood must not churn metric series; probes
//     are opted out so monitoring stays up during a flood.
//   - metrics then recovery, innermost around h: a panic is recovered, logged
//     with a stack, and answered 500 — and because recovery sits inside
//     metrics, that 500 is counted like any other status.
//
// limiter is the rate-limiting middleware (quota.NewRateLimiter(...).Middleware)
// built from config; it is a parameter because it carries config-driven state.
// The order of the layers is pinned by StandardStackOrder in stack_test.go.
func StandardStack(h http.Handler, log *slog.Logger, metrics *Metrics, limiter Middleware) http.Handler {
	return Chain(h,
		RequestID(log),
		AccessLog,
		limiter,
		metrics.Middleware,
		Recovery,
	)
}
