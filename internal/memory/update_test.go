package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
)

// Update and PurgeDeprecated are the write-side additions consolidation needs
// (memory-consolidation): revising a memory in place instead of appending a
// near-duplicate, and closing the window on deprecated rows so the store stops
// growing in memories nothing can recall. Both implementations must agree, or
// behaviour changes with whichever port is wired.

func TestMemPortUpdate(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()

	m, err := p.Store(ctx, Memory{
		Scope:     identity.UserScope("u1"),
		Kind:      KindFact,
		Content:   "user is planning a trip",
		Embedding: []float32{0.1, 0.2, 0.3},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	// Force a distinguishable clock gap; UpdatedAt must move, CreatedAt must not.
	time.Sleep(2 * time.Millisecond)

	if err := p.Update(ctx, m.ID, "user took the trip in July 2026"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := p.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Content != "user took the trip in July 2026" {
		t.Errorf("content = %q, want the revised text", got.Content)
	}
	if got.ID != m.ID {
		t.Errorf("id = %q, want it unchanged (%q)", got.ID, m.ID)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Errorf("CreatedAt = %v, want it unchanged (%v)", got.CreatedAt, m.CreatedAt)
	}
	if !got.UpdatedAt.After(m.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want it after the original (%v)", got.UpdatedAt, m.UpdatedAt)
	}
	// The embedding described the old text. Keeping it would make vector recall
	// rank this memory by a sentence it no longer contains.
	if got.Embedding != nil {
		t.Errorf("embedding = %v, want nil after a content revision", got.Embedding)
	}
}

func TestMemPortUpdateNotFound(t *testing.T) {
	p := NewMemPort()
	if err := p.Update(context.Background(), "no-such-id", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMemPortPurgeDeprecated(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()

	live, err := p.Store(ctx, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "live"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := p.Store(ctx, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "long retired"})
	if err != nil {
		t.Fatal(err)
	}
	recent, err := p.Store(ctx, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "just retired"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{old.ID, recent.ID} {
		if err := p.Deprecate(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate only the old one's deprecation. Deprecate stamps UpdatedAt, which
	// is what dates a deprecation, so reach in and age it.
	p.mu.Lock()
	p.memories[old.ID].UpdatedAt = time.Now().Add(-48 * time.Hour)
	p.mu.Unlock()

	n, err := p.PurgeDeprecated(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeDeprecated: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if _, err := p.GetByID(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Error("the long-retired memory should be gone")
	}
	// A live memory is never touched, however old — the cutoff is about
	// deprecation, not age.
	if _, err := p.GetByID(ctx, live.ID); err != nil {
		t.Errorf("live memory was purged: %v", err)
	}
	if _, err := p.GetByID(ctx, recent.ID); err != nil {
		t.Errorf("recently retired memory was purged before its window closed: %v", err)
	}
}

func TestPGPortUpdate(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()

	m, err := p.Store(ctx, Memory{
		Scope:     identity.UserScope("11111111-1111-1111-1111-111111111111"),
		Kind:      KindFact,
		Content:   "user is planning a trip",
		Embedding: []float32{0.1, 0.2, 0.3},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	cleanup(t, db, m.ID)

	if err := p.Update(ctx, m.ID, "user took the trip in July 2026"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := p.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Content != "user took the trip in July 2026" {
		t.Errorf("content = %q, want the revised text", got.Content)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Errorf("CreatedAt = %v, want it unchanged (%v)", got.CreatedAt, m.CreatedAt)
	}
	if !got.UpdatedAt.After(m.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want it after the original (%v)", got.UpdatedAt, m.UpdatedAt)
	}
	if got.Embedding != nil {
		t.Errorf("embedding = %v, want nil after a content revision", got.Embedding)
	}
}

func TestPGPortUpdateNotFound(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	err := p.Update(context.Background(), "00000000-0000-0000-0000-000000000000", "x")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Ids reach this port from URL path segments and from LLM output. A malformed
// one names nothing, so it is a miss — not a 22P02 surfaced as a server fault.
func TestPGPortUpdateMalformedID(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	err := p.Update(context.Background(), "not-a-uuid", "x")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a malformed id", err)
	}
}

// PurgeDeprecated is a global sweep by design — housekeeping, not a per-scope
// operation. pgTestDB points at the developer's own database, so this test
// backdates its row to an era no real data occupies and sets the cutoff there,
// making the DELETE incapable of matching anything it did not create. An
// earlier version used a now-relative cutoff and deleted 99 real deprecated
// rows; a test must not be able to reach outside its own fixtures.
func TestPGPortPurgeDeprecated(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.UserScope("22222222-2222-2222-2222-222222222222")

	// An era no production row occupies.
	const ancient = "2000-01-01 00:00:00+00"
	cutoff := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

	live, err := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "live"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, db, live.ID)
	old, err := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "long retired"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, db, old.ID)
	recent, err := p.Store(ctx, Memory{Scope: scope, Kind: KindFact, Content: "just retired"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, db, recent.ID)

	for _, id := range []string{old.ID, recent.ID} {
		if err := p.Deprecate(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// Only the "long retired" row is aged past the cutoff. The live row is
	// backdated too, to prove the sweep keys on deprecation and not on age.
	for _, id := range []string{old.ID, live.ID} {
		if _, err := db.ExecContext(ctx,
			`UPDATE memories SET updated_at = $2::timestamptz WHERE id = $1`, id, ancient); err != nil {
			t.Fatal(err)
		}
	}

	n, err := p.PurgeDeprecated(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeDeprecated: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want exactly 1 — the sweep reached beyond this test's fixtures", n)
	}
	if _, err := p.GetByID(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Error("the long-retired memory should be gone")
	}
	// Old but live: never purged, however far back it is dated.
	if _, err := p.GetByID(ctx, live.ID); err != nil {
		t.Errorf("live memory was purged: %v", err)
	}
	if _, err := p.GetByID(ctx, recent.ID); err != nil {
		t.Errorf("recently retired memory was purged before its window closed: %v", err)
	}
}
