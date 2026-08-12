package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestOpenRejectsMalformedDSN verifies a DSN that cannot be parsed is refused
// with a wrapped error rather than silently opened. pgx parses lazily at the
// first connection, so the failure surfaces at the probe, wrapped as
// "ping db:" (the wrapping — not the inner message — is what callers key on).
// No database is touched: the parse fails before any socket is attempted.
func TestOpenRejectsMalformedDSN(t *testing.T) {
	for _, dsn := range []string{"://bad%zz", "not-a-dsn"} {
		_, err := Open(context.Background(), dsn, 1, 1, time.Minute)
		if err == nil {
			t.Errorf("Open(%q) succeeded, want an error", dsn)
			continue
		}
		if !strings.Contains(err.Error(), "ping db:") {
			t.Errorf("Open(%q) err = %v, want wrapped as 'ping db: …'", dsn, err)
		}
	}
}

// TestOpenRefusesUnreachableDSN verifies the connectivity probe: a DSN whose
// host refuses connections fails at Ping, wrapped as "ping db:", so callers
// can tell a misconfigured DSN from a dead database. Loopback port 1 refuses
// every connection, so no real database is involved.
func TestOpenRefusesUnreachableDSN(t *testing.T) {
	_, err := Open(context.Background(), "postgres://u:p@127.0.0.1:1/nowhere?sslmode=disable", 2, 2, time.Minute)
	if err == nil {
		t.Fatal("Open to a closed port succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "ping db:") {
		t.Errorf("err = %v, want wrapped as 'ping db: …'", err)
	}
}
