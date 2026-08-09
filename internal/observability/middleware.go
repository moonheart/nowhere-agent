package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
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
	return context.WithValue(ctx, loggerKey, log)
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
// 500. When the handler already started streaming (SSE), the response cannot be
// rewritten, so Recovery just severs the connection by returning; the run layer
// settles the run independently. Place it directly around the mux, INSIDE the
// metrics middleware, so the 500 it writes is counted like any other status.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				loggerFor(r.Context()).Error("http handler panicked",
					"panic", p, "stack", string(debug.Stack()))
				if wr, ok := w.(interface{ Written() bool }); !ok || !wr.Written() {
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessRecorder captures status, bytes written, and time-to-first-byte for the
// access log, independently of the metrics recorder (they sit at different
// layers of the stack and must not share state).
type accessRecorder struct {
	http.ResponseWriter
	start  time.Time
	status int
	bytes  int
	ttfb   time.Duration
	wrote  bool
}

func (a *accessRecorder) WriteHeader(code int) {
	if !a.wrote {
		a.wrote = true
		a.status = code
		a.ttfb = time.Since(a.start)
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *accessRecorder) Write(b []byte) (int, error) {
	if !a.wrote {
		a.WriteHeader(http.StatusOK)
	}
	n, err := a.ResponseWriter.Write(b)
	a.bytes += n
	return n, err
}

// Flush forwards streaming flushes (SSE asserts http.Flusher directly; a type
// assertion does not traverse Unwrap).
func (a *accessRecorder) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (a *accessRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// AccessLog emits one structured log line per completed request: status,
// latency, time-to-first-byte, and response bytes, on the request-scoped
// logger (so the line carries request_id/method/path for free). A client that
// disconnected mid-handler is logged as 499, which is what makes SSE stream
// endings visible. Placed OUTSIDE the rate limiter, it also sees rejected
// requests (429), closing the blind spot where throttled traffic left no trace.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &accessRecorder{ResponseWriter: w, start: start, status: http.StatusOK}
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
