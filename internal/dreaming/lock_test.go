package dreaming

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/session"
)

// These tests run against the shared development Postgres (skipping when none
// is reachable), the repo's convention. They create no rows — the advisory
// lock lives on the connection, not in a table — so no cleanup is needed.

func lockTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The lock is connection-scoped and held for the whole pass, so a second
// instance's TryAcquire must fail until the first releases — even though both
// go through the same pool. This is the multi-instance contract: only the
// holder may consolidate.
func TestPGAdvisoryLockContends(t *testing.T) {
	db := lockTestDB(t)
	ctx := context.Background()

	a := NewPGAdvisoryLock(db)
	b := NewPGAdvisoryLock(db)

	ok, err := a.TryAcquire(ctx)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v, want ok", ok, err)
	}
	ok, err = b.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if ok {
		t.Fatal("second instance acquired the lock while the first holds it")
	}

	if err := a.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err = b.TryAcquire(ctx)
	if err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v, want ok", ok, err)
	}
	// A release is idempotent on the holder and must not disturb the new holder.
	if err := b.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := b.Release(); err != nil {
		t.Fatalf("second release: %v (want a no-op)", err)
	}
}

// Two runners on different "instances" (each with its own lock object) must
// behave like one: while instance A holds the pass, B's scheduled tick skips
// and B's manual trigger reports ErrBusy — the episodes are consolidated at
// most once. When A finishes, B runs normally.
func TestRunnerCrossInstanceLockSkipsWhileLocked(t *testing.T) {
	db := lockTestDB(t)

	blocking := newBlockingSource(&fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	})
	a := newTestRunner(t, blocking, memory.NewMemPort(), &fakeLLM{tokens: 1})
	a.SetLock(NewPGAdvisoryLock(db))
	if err := a.TriggerForUser("u1"); err != nil {
		t.Fatal(err)
	}
	<-blocking.entered // instance A's pass is now genuinely in flight

	// Instance B: the scheduled pass must skip (not fail, not run).
	llmB := &fakeLLM{tokens: 1}
	b := newTestRunner(t, &fakeEpisodeSource{}, memory.NewMemPort(), llmB)
	b.SetLock(NewPGAdvisoryLock(db))
	if err := b.RunScheduled(context.Background()); err != nil {
		t.Errorf("scheduled pass on instance B returned %v, want nil (a skip is not an error)", err)
	}
	if llmB.calls != 0 {
		t.Errorf("instance B's scheduled pass ran anyway: %d LLM calls", llmB.calls)
	}
	// And the manual trigger is told to wait, exactly like local contention.
	if err := b.TriggerForUser("u1"); !errors.Is(err, ErrBusy) {
		t.Errorf("instance B trigger err = %v, want ErrBusy while A holds the pass", err)
	}

	close(blocking.release)
	a.Wait()

	// Once A released the lock, B's next pass runs — the skip was a skip, not
	// a one-way door.
	if err := b.RunScheduled(context.Background()); err != nil {
		t.Fatalf("instance B scheduled pass after A released: %v", err)
	}
}
