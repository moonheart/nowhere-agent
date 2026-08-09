package reqctx

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	if RequestID(ctx) != "" || Logger(ctx) != nil || SessionID(ctx) != "" {
		t.Error("empty context must yield zero values")
	}
	if _, ok := User(ctx); ok {
		t.Error("empty context must have no user")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx = WithRequestID(ctx, "req-1")
	ctx = WithLogger(ctx, log)
	ctx = WithUser(ctx, "user-abc")
	ctx = WithSessionID(ctx, "sess-9")

	if RequestID(ctx) != "req-1" {
		t.Errorf("RequestID = %q, want req-1", RequestID(ctx))
	}
	if Logger(ctx) != log {
		t.Error("Logger lost the injected logger")
	}
	if u, ok := User(ctx); !ok || u != "user-abc" {
		t.Errorf("User = %v, %v; want user-abc, true", u, ok)
	}
	if SessionID(ctx) != "sess-9" {
		t.Errorf("SessionID = %q, want sess-9", SessionID(ctx))
	}
}

func TestDetachKeepsValuesNotCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx = WithRequestID(ctx, "req-2")
	ctx = WithLogger(ctx, log)
	ctx = WithUser(ctx, "user-x")
	ctx = WithSessionID(ctx, "sess-y")
	cancel()

	run := Detach(ctx)
	if run.Err() != nil {
		t.Fatalf("detached context inherited cancellation: %v", run.Err())
	}
	if RequestID(run) != "req-2" {
		t.Errorf("RequestID after detach = %q, want req-2", RequestID(run))
	}
	if Logger(run) != log {
		t.Error("Logger after detach was lost")
	}
	if u, ok := User(run); !ok || u != "user-x" {
		t.Errorf("User after detach = %v, %v", u, ok)
	}
	if SessionID(run) != "sess-y" {
		t.Errorf("SessionID after detach = %q, want sess-y", SessionID(run))
	}
}

func TestDetachNilLogger(t *testing.T) {
	run := Detach(WithRequestID(context.Background(), "req-3"))
	if Logger(run) != nil {
		t.Error("Detach must not invent a logger for a context that had none")
	}
}
