package dreaming

import (
	"context"
	"database/sql"
	"time"
)

// passLockKey is the fixed 64-bit key of the cross-instance pass lock. It is a
// constant so every instance contends on the SAME advisory lock; a per-instance
// key would let two instances run overlapping passes and re-race the
// watermarks the in-memory single-flight lock stops within one process.
const passLockKey int64 = 0x6E6F7768657265 // "nowhere"

// Lock is the cross-instance mutual exclusion over consolidation passes. The
// runner takes it BEFORE the pass reads any watermarks and holds it until the
// pass finishes, so a second instance's TryAcquire fails for the whole pass —
// not just for its eligibility scan (two passes on different instances would
// both read a session's dreamed_seq before either advances it, and each would
// consolidate the same episodes into a duplicate set of memories).
type Lock interface {
	// TryAcquire takes the lock. ok is false when another holder has it; the
	// caller then skips the pass (a missed tick is not an error).
	TryAcquire(ctx context.Context) (ok bool, err error)
	// Release drops the lock. It must be called exactly once after a successful
	// TryAcquire, on the same connection that took it — Postgres advisory locks
	// are connection-scoped.
	Release() error
}

// PGAdvisoryLock backs Lock with pg_try_advisory_lock on a dedicated
// connection: the connection is checked out of the pool at TryAcquire and
// returned at Release, so the lock survives every intermediate query on other
// connections and dies with the pass, not with any single statement.
type PGAdvisoryLock struct {
	db   *sql.DB
	conn *sql.Conn
}

// NewPGAdvisoryLock wires the advisory lock over the platform pool.
func NewPGAdvisoryLock(db *sql.DB) *PGAdvisoryLock { return &PGAdvisoryLock{db: db} }

// TryAcquire takes the advisory lock on a connection owned by the lock object.
func (l *PGAdvisoryLock) TryAcquire(ctx context.Context) (bool, error) {
	if l.conn != nil {
		// Already held by this object. The runner never calls TryAcquire twice
		// on one lock, so this is defensive only.
		return true, nil
	}
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, passLockKey).Scan(&got); err != nil {
		conn.Close()
		return false, err
	}
	if !got {
		conn.Close()
		return false, nil
	}
	l.conn = conn
	return true, nil
}

// Release drops the lock. A failed unlock is not fatal: the connection is
// closed regardless, and closing a connection releases every advisory lock it
// holds, so the pass can never be starved by a stuck unlock.
func (l *PGAdvisoryLock) Release() error {
	if l.conn == nil {
		return nil
	}
	// A short-lived context: the pass may have ended while the process shuts
	// down, and the release must not depend on the pass's own (cancelled) ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, passLockKey)
	l.conn.Close()
	l.conn = nil
	return err
}
