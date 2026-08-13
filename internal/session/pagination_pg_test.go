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

	p1, err := store.ListSessionsByUser(ctx, userID, "", 2, nil)
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

	p2, err := store.ListSessionsByUser(ctx, userID, "", 2, p1.NextCursor)
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

// TestPGStoreListSessionsSearch verifies the sidebar title search against real
// Postgres: q narrows case-insensitively, LIKE wildcards in the term are
// matched literally, and a no-match query returns an empty nil-cursor page.
func TestPGStoreListSessionsSearch(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	for _, title := range []string{"Alpha Plan", "beta report", "chat 42", "100% done"} {
		if _, err := store.CreateSession(ctx, userID, title); err != nil {
			t.Fatal(err)
		}
	}

	// Case-insensitive contains.
	p, err := store.ListSessionsByUser(ctx, userID, "ALPHA", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sessions) != 1 || p.Sessions[0].Title != "Alpha Plan" {
		t.Fatalf("q=ALPHA -> %d sessions (%q), want just Alpha Plan", len(p.Sessions), titlesOf(p))
	}

	// '%' is a literal in the term, not a wildcard: "0%" matches only the
	// title that literally contains it.
	p, err = store.ListSessionsByUser(ctx, userID, "0%", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sessions) != 1 || p.Sessions[0].Title != "100% done" {
		t.Fatalf("q=0%% -> %d sessions (%q), want just 100%% done", len(p.Sessions), titlesOf(p))
	}

	// A '_' wildcard must not widen the match either.
	p, err = store.ListSessionsByUser(ctx, userID, "chat_42", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sessions) != 0 {
		t.Fatalf("q=chat_42 -> %d sessions, want 0 (underscore is literal)", len(p.Sessions))
	}

	// No matches: empty page, nil cursor (list exhausted).
	p, err = store.ListSessionsByUser(ctx, userID, "nope", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sessions) != 0 || p.NextCursor != nil {
		t.Fatalf("q=nope -> %d sessions, cursor=%v; want empty + nil cursor", len(p.Sessions), p.NextCursor != nil)
	}
}

func titlesOf(p SessionPage) []string {
	out := make([]string, 0, len(p.Sessions))
	for _, s := range p.Sessions {
		out = append(out, s.Title)
	}
	return out
}
