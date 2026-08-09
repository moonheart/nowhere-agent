package session

import (
	"context"
	"testing"
)

// TestPGStoreListSessionsPagination verifies keyset pagination against real
// Postgres: newest-first pages of the requested size, a cursor that continues
// where the previous page ended, and a nil cursor once the list is exhausted.
func TestPGStoreListSessionsPagination(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	// Three sessions with distinct updated_at, oldest first by creation order
	// (i=0 oldest, i=2 newest).
	var ids []string
	for i := 0; i < 3; i++ {
		s, err := store.CreateSession(ctx, userID, "page")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
		if _, err := db.Exec(
			`UPDATE sessions SET updated_at = now() - make_interval(mins => $2) WHERE id = $1`,
			s.ID, 3-i,
		); err != nil {
			t.Fatal(err)
		}
	}

	p1, err := store.ListSessionsByUser(ctx, userID, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Sessions) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 = %d sessions, cursor set = %v", len(p1.Sessions), p1.NextCursor != nil)
	}
	if p1.Sessions[0].ID != ids[2] {
		t.Errorf("page 1 first = %s want %s (newest)", p1.Sessions[0].ID, ids[2])
	}
	if p1.Sessions[1].ID != ids[1] {
		t.Errorf("page 1 second = %s want %s", p1.Sessions[1].ID, ids[1])
	}

	p2, err := store.ListSessionsByUser(ctx, userID, 2, p1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Sessions) != 1 || p2.NextCursor != nil {
		t.Fatalf("page 2 = %d sessions, cursor set = %v", len(p2.Sessions), p2.NextCursor != nil)
	}
	if p2.Sessions[0].ID != ids[0] {
		t.Errorf("page 2 first = %s want %s (oldest)", p2.Sessions[0].ID, ids[0])
	}
}
