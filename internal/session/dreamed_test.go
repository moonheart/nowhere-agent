package session

import (
	"context"
	"testing"
)

// TestMemStoreEndedUndreamed pins the dreaming worker's eligibility scan: only
// ended, not-yet-dreamed sessions are returned; MarkDreamed takes one out of
// the result; active and already-dreamed sessions never appear.
func TestMemStoreEndedUndreamed(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()

	a, _ := m.CreateSession(ctx, "u1", "a")
	b, _ := m.CreateSession(ctx, "u1", "b")
	c, _ := m.CreateSession(ctx, "u2", "c")

	// Nothing ended yet.
	if got, _ := m.ListEndedUndreamed(ctx); len(got) != 0 {
		t.Fatalf("no ended sessions expected, got %d", len(got))
	}

	_ = m.EndSession(ctx, a.ID)
	_ = m.EndSession(ctx, b.ID)
	// c stays active — must never be eligible.

	got, err := m.ListEndedUndreamed(ctx)
	if err != nil {
		t.Fatalf("ListEndedUndreamed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 ended-undreamed, got %d", len(got))
	}

	// Marking one removes it; the other remains; re-marking is a harmless no-op.
	if err := m.MarkDreamed(ctx, a.ID); err != nil {
		t.Fatalf("MarkDreamed: %v", err)
	}
	if err := m.MarkDreamed(ctx, a.ID); err != nil {
		t.Fatalf("MarkDreamed idempotent: %v", err)
	}
	got, _ = m.ListEndedUndreamed(ctx)
	if len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("after marking a, want only b, got %+v", got)
	}
	for _, s := range got {
		if s.ID == c.ID {
			t.Errorf("active session %s must not be eligible", c.ID)
		}
	}
}

// TestPGStoreEndedUndreamed exercises the same behaviour against Postgres. It
// only asserts on the sessions this test created (the dev DB is shared), and
// skips when no database is reachable.
func TestPGStoreEndedUndreamed(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	ended, err := store.CreateSession(ctx, userID, "ended")
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.CreateSession(ctx, userID, "active")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(ctx, ended.ID); err != nil {
		t.Fatal(err)
	}

	contains := func(list []Session, id string) bool {
		for _, s := range list {
			if s.ID == id {
				return true
			}
		}
		return false
	}

	got, err := store.ListEndedUndreamed(ctx)
	if err != nil {
		t.Fatalf("ListEndedUndreamed: %v", err)
	}
	if !contains(got, ended.ID) {
		t.Errorf("ended session %s should be eligible for dreaming", ended.ID)
	}
	if contains(got, active.ID) {
		t.Errorf("active session %s must not be eligible", active.ID)
	}

	if err := store.MarkDreamed(ctx, ended.ID); err != nil {
		t.Fatalf("MarkDreamed: %v", err)
	}
	got, _ = store.ListEndedUndreamed(ctx)
	if contains(got, ended.ID) {
		t.Errorf("dreamed session %s should drop out of the eligibility scan", ended.ID)
	}
}
