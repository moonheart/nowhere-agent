package session

import (
	"context"
	"testing"
	"time"
)

// TestMemStoreMemoryInjectedAt pins the in-memory memory-injection watermark:
// it starts at the zero time, advances monotonically, and never moves backwards.
func TestMemStoreMemoryInjectedAt(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	s, _ := m.CreateSession(ctx, "u1", "a")

	if got, _ := m.MemoryInjectedAt(ctx, s.ID); !got.IsZero() {
		t.Fatalf("initial memory_injected_at = %v want zero", got)
	}

	t1 := time.Now()
	if err := m.MarkMemoryInjectedAt(ctx, s.ID, t1); err != nil {
		t.Fatalf("MarkMemoryInjectedAt: %v", err)
	}
	if got, _ := m.MemoryInjectedAt(ctx, s.ID); !got.Equal(t1) {
		t.Errorf("memory_injected_at = %v want %v", got, t1)
	}

	// Never backwards.
	if err := m.MarkMemoryInjectedAt(ctx, s.ID, t1.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkMemoryInjectedAt backwards: %v", err)
	}
	if got, _ := m.MemoryInjectedAt(ctx, s.ID); !got.Equal(t1) {
		t.Errorf("memory_injected_at regressed to %v, want it to stay %v", got, t1)
	}

	// Advances forwards.
	t2 := t1.Add(time.Hour)
	if err := m.MarkMemoryInjectedAt(ctx, s.ID, t2); err != nil {
		t.Fatalf("MarkMemoryInjectedAt fwd: %v", err)
	}
	if got, _ := m.MemoryInjectedAt(ctx, s.ID); !got.Equal(t2) {
		t.Errorf("memory_injected_at = %v want %v", got, t2)
	}

	// Unknown session reads as zero.
	if got, _ := m.MemoryInjectedAt(ctx, "nope"); !got.IsZero() {
		t.Errorf("unknown session memory_injected_at = %v want zero", got)
	}
}

// TestPGStoreMemoryInjectedAt exercises the watermark against Postgres: NULL
// reads as zero, GREATEST keeps it monotonic. Skips when no database is
// reachable.
func TestPGStoreMemoryInjectedAt(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	sess, err := store.CreateSession(ctx, userID, "wm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID)
	})

	if got, err := store.MemoryInjectedAt(ctx, sess.ID); err != nil || !got.IsZero() {
		t.Fatalf("initial memory_injected_at = %v err %v want zero", got, err)
	}

	t1 := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.MarkMemoryInjectedAt(ctx, sess.ID, t1); err != nil {
		t.Fatalf("MarkMemoryInjectedAt: %v", err)
	}
	if got, _ := store.MemoryInjectedAt(ctx, sess.ID); got.Before(t1) {
		t.Errorf("memory_injected_at = %v want >= %v", got, t1)
	}

	// Never backwards.
	if err := store.MarkMemoryInjectedAt(ctx, sess.ID, t1.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkMemoryInjectedAt backwards: %v", err)
	}
	if got, _ := store.MemoryInjectedAt(ctx, sess.ID); got.Before(t1) {
		t.Errorf("memory_injected_at regressed to %v, want >= %v", got, t1)
	}
}
