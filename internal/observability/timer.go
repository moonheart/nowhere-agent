package observability

import (
	"context"
	"time"
)

// Timed runs fn with a threshold timer (the analogue of langgraph_api's
// timer.py): the duration is measured, and a warn/error line is emitted on the
// request-scoped logger when it crosses the named thresholds. warn and error are
// time.Durations; a zero warn disables the warning line, a zero error disables
// the error line. The returned duration lets the caller observe even the
// sub-threshold case (e.g. to feed a histogram).
//
// Use it around operations whose slowness is a signal on the hot path — a DB
// write, a tool dispatch — where a bare slog call would require the caller to
// re-implement "measure then classify" every time.
func Timed(ctx context.Context, name string, warn, error time.Duration, fn func()) time.Duration {
	start := time.Now()
	fn()
	elapsed := time.Since(start)
	switch {
	case error > 0 && elapsed >= error:
		loggerFor(ctx).Error("slow operation exceeded error threshold",
			"op", name, "elapsed_ms", elapsed.Milliseconds(), "threshold_ms", error.Milliseconds())
	case warn > 0 && elapsed >= warn:
		loggerFor(ctx).Warn("slow operation exceeded warn threshold",
			"op", name, "elapsed_ms", elapsed.Milliseconds(), "threshold_ms", warn.Milliseconds())
	}
	return elapsed
}

// TimedFunc is Timed returning a value (so call sites can time a function that
// produces a result without stashing it in a closure variable).
func TimedFunc[T any](ctx context.Context, name string, warn, error time.Duration, fn func() T) (T, time.Duration) {
	var v T
	elapsed := Timed(ctx, name, warn, error, func() { v = fn() })
	return v, elapsed
}
