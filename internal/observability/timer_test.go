package observability

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestTimedThresholds pins the warn/error classification: a sub-threshold op
// logs nothing, crossing warn logs a warning, crossing error logs an error,
// and the elapsed duration is always returned to the caller.
func TestTimedThresholds(t *testing.T) {
	newCtx := func(buf *bytes.Buffer) context.Context {
		log := slog.New(slog.NewTextHandler(buf, nil))
		return WithLogger(context.Background(), log)
	}

	t.Run("sub-threshold logs nothing", func(t *testing.T) {
		var buf bytes.Buffer
		elapsed := Timed(newCtx(&buf), "op", time.Hour, 2*time.Hour, func() {})
		if buf.String() != "" {
			t.Errorf("sub-threshold op logged: %s", buf.String())
		}
		if elapsed < 0 {
			t.Error("elapsed must be non-negative")
		}
	})

	t.Run("warn threshold crossed", func(t *testing.T) {
		var buf bytes.Buffer
		Timed(newCtx(&buf), "op", time.Nanosecond, time.Hour, func() { time.Sleep(time.Millisecond) })
		if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "op=op") {
			t.Errorf("warn crossing not logged as WARN: %s", out)
		}
	})

	t.Run("error threshold crossed", func(t *testing.T) {
		var buf bytes.Buffer
		Timed(newCtx(&buf), "op", time.Nanosecond, time.Nanosecond, func() { time.Sleep(time.Millisecond) })
		if out := buf.String(); !strings.Contains(out, "level=ERROR") {
			t.Errorf("error crossing not logged as ERROR: %s", out)
		}
	})

	t.Run("zero thresholds disable", func(t *testing.T) {
		var buf bytes.Buffer
		Timed(newCtx(&buf), "op", 0, 0, func() { time.Sleep(time.Millisecond) })
		if buf.String() != "" {
			t.Errorf("zero thresholds must disable both lines: %s", buf.String())
		}
	})
}

// TestTimedFuncReturnsValue pins the value-returning variant.
func TestTimedFuncReturnsValue(t *testing.T) {
	v, elapsed := TimedFunc(context.Background(), "op", 0, 0, func() int { return 42 })
	if v != 42 {
		t.Errorf("value = %d, want 42", v)
	}
	if elapsed < 0 {
		t.Error("elapsed must be non-negative")
	}
}
