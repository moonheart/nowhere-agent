package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/reqctx"
)

// Middleware is the standard http middleware shape. Chain composes them.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares around h, first argument outermost. It is the
// single assembly point for the inbound stack (the HTTP analogue of the agent
// loop's useStandardMiddleware), so the order invariants live in exactly one
// place instead of being re-nested by hand at every change.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// WithLogger returns a context carrying log, retrievable via LoggerFromContext.
// RequestID uses it to inject the request-scoped logger; background workers
// (run goroutines) use it to propagate that logger past the HTTP boundary.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return reqctx.WithLogger(ctx, log)
}

// loggerFor returns the context's scoped logger, or the process default, so
// middleware can log unconditionally whether or not RequestID ran upstream.
func loggerFor(ctx context.Context) *slog.Logger {
	if log := LoggerFromContext(ctx); log != nil {
		return log
	}
	return slog.Default()
}

// StatusClientClosed is the nginx-style pseudo status logged (and counted) when
// the client went away before the handler finished — the common end of an SSE
// stream. It distinguishes "we answered" from "they left" in access logs.
const StatusClientClosed = 499

// clientGone reports whether the client disconnected before the handler
// returned: the request context is cancelled and the cancellation did not come
// from a server-side deadline.
func clientGone(r *http.Request) bool {
	return errors.Is(r.Context().Err(), context.Canceled)
}

// Recovery catches a panic in the wrapped handler, logs it with a stack on the
// request-scoped logger, and — only if no status was committed yet — answers
// 500 with the standard JSON error body. When the handler already started
// streaming (SSE), the response cannot be rewritten, so Recovery just severs
// the connection by returning; the run layer settles the run independently.
// Place it directly around the mux, INSIDE the metrics middleware, so the 500
// it writes is counted like any other status. A panicked error value that
// carries an HTTP status (see httpx.StatusFor) is answered with that status
// instead of a blanket 500, so the panic boundary doubles as the "known errors
// get normalized" boundary.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				loggerFor(r.Context()).Error("http handler panicked",
					"panic", p, "stack", string(debug.Stack()))
				if wr, ok := w.(interface{ Written() bool }); !ok || !wr.Written() {
					httpx.Error(w, httpx.StatusFor(p), "internal server error")
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets the baseline security response headers on every
// response: content-type sniffing off (X-Content-Type-Options), referrer
// containment (Referrer-Policy), and frame denial (X-Frame-Options). Placed
// outside the rate limiter so even throttled responses carry them.
//
// A Content-Security-Policy is deliberately NOT set here: the SPA renders
// inline styles through React style attributes and library-injected <style>
// blocks, which a strict style-src would block and a permissive one would
// weaken to near-uselessness; shipping a policy that can only be sent with
// 'unsafe-inline' deserves a dedicated pass over the whole UI, not a silent
// relaxation in a baseline middleware.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// AccessLog emits one structured log line per completed request: status,
// latency, time-to-first-byte, and response bytes, on the request-scoped
// logger (so the line carries request_id/method/path for free). A client that
// disconnected mid-handler is logged as 499, which is what makes SSE stream
// endings visible. Placed OUTSIDE the rate limiter, it also sees rejected
// requests (429), closing the blind spot where throttled traffic left no trace.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := newStatusWriter(w, start)
		next.ServeHTTP(rec, r)

		status := rec.status
		if clientGone(r) {
			status = StatusClientClosed
		}
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		attrs := []slog.Attr{
			slog.Int("status", status),
			slog.String("route", route),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			slog.Int("bytes", rec.bytes),
		}
		if rec.wrote {
			attrs = append(attrs, slog.Int64("ttfb_ms", rec.ttfb.Milliseconds()))
		}
		level := slog.LevelInfo
		switch {
		case status >= 500 && status != StatusClientClosed:
			level = slog.LevelError
		case status == StatusClientClosed || status >= 400:
			level = slog.LevelWarn
		}
		loggerFor(r.Context()).LogAttrs(r.Context(), level, "http request", attrs...)
	})
}
