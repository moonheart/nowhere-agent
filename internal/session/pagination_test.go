package session

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// TestMemStoreListSessionsPagination walks a 30-session list with limit 10 and
// verifies keyset pagination: newest-first ordering, disjoint pages covering
// every session, other users' and ended sessions excluded, and a nil cursor on
// the final page.
func TestMemStoreListSessionsPagination(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()

	base := time.Now().Add(-30 * time.Minute)
	ids := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		s, err := m.CreateSession(ctx, "alice", fmt.Sprintf("chat %d", i))
		if err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		m.sessions[s.ID].UpdatedAt = base.Add(time.Duration(i) * time.Minute)
		m.mu.Unlock()
		ids = append(ids, s.ID)
	}
	other, err := m.CreateSession(ctx, "bob", "bob's chat")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions[other.ID].UpdatedAt = base.Add(100 * time.Minute)
	m.mu.Unlock()
	ended, err := m.CreateSession(ctx, "alice", "ended chat")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions[ended.ID].Status = SessionEnded
	m.sessions[ended.ID].UpdatedAt = base.Add(101 * time.Minute)
	m.mu.Unlock()

	var got []string
	var cursor *SessionCursor
	pages := 0
	for {
		p, err := m.ListSessionsByUser(ctx, "alice", "", 10, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Sessions {
			got = append(got, s.ID)
		}
		pages++
		if p.NextCursor == nil {
			break
		}
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
		cursor = p.NextCursor
	}

	if pages != 3 {
		t.Errorf("walked %d pages, want 3 (limit 10 over 30)", pages)
	}
	if len(got) != 30 {
		t.Fatalf("walked %d sessions, want 30", len(got))
	}
	// Newest first: last-created session (largest updated_at) leads the list.
	if got[0] != ids[29] {
		t.Errorf("first = %s want %s (newest)", got[0], ids[29])
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Errorf("duplicate session %s across pages", id)
		}
		if id == other.ID || id == ended.ID {
			t.Errorf("foreign/ended session %s leaked into the list", id)
		}
		seen[id] = true
	}
}

// TestMemStoreListSessionsKeysetTiebreak pins the id tiebreaker: sessions
// updated in the same instant order by id DESC, and the cursor still walks them
// exactly once.
func TestMemStoreListSessionsKeysetTiebreak(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()
	now := time.Now()
	var ids []string
	for i := 0; i < 4; i++ {
		s, err := m.CreateSession(ctx, "alice", "tie")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
		m.mu.Lock()
		m.sessions[s.ID].UpdatedAt = now
		m.mu.Unlock()
	}

	want := make([]string, len(ids))
	copy(want, ids)
	sort.Sort(sort.Reverse(sort.StringSlice(want)))

	var got []string
	var cursor *SessionCursor
	for {
		p, err := m.ListSessionsByUser(ctx, "alice", "", 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Sessions {
			got = append(got, s.ID)
		}
		if p.NextCursor == nil {
			break
		}
		cursor = p.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d sessions, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("position %d = %s want %s (id-desc tiebreak)", i, got[i], want[i])
		}
	}
}
