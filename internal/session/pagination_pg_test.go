package session

import (
	"context"
	"slices"
	"testing"

	"nowhere-agent/internal/provider"
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

// TestPGStoreListSessionsSearchAcrossPages covers the gap the single-page
// search tests leave: a search whose hits span pages. The q term must travel
// with the keyset cursor — page 2 continues the SAME filtered set, so the
// walk covers every matching session exactly once and non-matches never
// appear on either page.
func TestPGStoreListSessionsSearchAcrossPages(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	// Three matches interleaved with three non-matches; updated_at orders them
	// i=0 newest, so the hits come back in creation order.
	for i, title := range []string{"needle one", "haystack", "needle two", "misc", "needle three", "other"} {
		s, err := store.CreateSession(ctx, userID, title)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`UPDATE sessions SET updated_at = now() - make_interval(mins => $2) WHERE id = $1`,
			s.ID, i+1,
		); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	p1, err := store.ListSessionsByUser(ctx, userID, "needle", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Sessions) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 = %d sessions, cursor set = %v", len(p1.Sessions), p1.NextCursor != nil)
	}
	got = append(got, titlesOf(p1)...)

	p2, err := store.ListSessionsByUser(ctx, userID, "needle", 2, p1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Sessions) != 1 || p2.NextCursor != nil {
		t.Fatalf("page 2 = %d sessions, cursor set = %v", len(p2.Sessions), p2.NextCursor != nil)
	}
	got = append(got, titlesOf(p2)...)

	if want := []string{"needle one", "needle two", "needle three"}; !slices.Equal(got, want) {
		t.Fatalf("search walk = %v, want %v (hits only, in order, no duplicates)", got, want)
	}
}

// TestPGStoreListSessionsSearchMatchesMessageContent verifies the migration
// 000049 leg of the sidebar search: a q that matches no title still returns
// the session when one of its messages' free text contains the term. Text,
// thinking, and tool_result blocks must all count; a term present only in
// JSON keys or tool-input payloads must NOT match; and the user's other
// sessions stay out.
func TestPGStoreListSessionsSearchMatchesMessageContent(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	msgStore := NewPGMessageStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	sessionFor := func(title string) (string, string) {
		t.Helper()
		s, err := store.CreateSession(ctx, userID, title)
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.CreateRun(ctx, s.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, s.ID) })
		return s.ID, run.ID
	}

	// Session A: title mentions nothing searchable; a user message holds the
	// needle in a text block (apricot is repeated so the pagination walk
	// below has a second match).
	sessA, runA := sessionFor("monday standup")
	_, err := msgStore.AppendMessage(ctx, StoredMessage{
		SessionID: sessA, RunID: runA, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "the pineapple and apricot deployment is green"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Session B: needle in a thinking block.
	sessB, runB := sessionFor("tuesday review")
	_, err = msgStore.AppendMessage(ctx, StoredMessage{
		SessionID: sessB, RunID: runB, Role: provider.RoleAssistant,
		Content: []provider.Block{{Type: provider.BlockThinking, Thinking: "check the avocado quota first"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Session C: needle in a tool_result's tool_content.
	sessC, runC := sessionFor("wednesday retro")
	_, err = msgStore.AppendMessage(ctx, StoredMessage{
		SessionID: sessC, RunID: runC, Role: provider.RoleAssistant,
		Content: []provider.Block{{
			Type: provider.BlockToolUse, ToolUseID: "u1", ToolName: "query_db",
			ToolInput: map[string]any{"sql": "SELECT 1"},
		}, {
			Type: provider.BlockToolResult, ToolUseID: "u1",
			ToolContent: "rows: mango sentinel",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Session D: the needle appears ONLY in a block ID field — identifiers
	// and key names are not free text, so it must NOT match.
	sessD, runD := sessionFor("thursday demo")
	_, err = msgStore.AppendMessage(ctx, StoredMessage{
		SessionID: sessD, RunID: runD, Role: provider.RoleAssistant,
		Content: []provider.Block{{
			Type: provider.BlockToolUse, ToolUseID: "grapefruit-1", ToolName: "query_db",
			ToolInput: map[string]any{"sql": "SELECT 1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for q, want := range map[string][]string{
		"pineapple":  {"monday standup"},
		"avocado":    {"tuesday review"},
		"mango":      {"wednesday retro"},
		"grapefruit": {},
	} {
		p, err := store.ListSessionsByUser(ctx, userID, q, 10, nil)
		if err != nil {
			t.Fatalf("q=%s: %v", q, err)
		}
		got := titlesOf(p)
		if !slices.Equal(got, want) {
			t.Errorf("q=%s -> %v, want %v", q, got, want)
		}
	}

	// The message leg must also paginate: two content-matched sessions with
	// one page of size 1 walk both without duplicating either.
	sessE, runE := sessionFor("friday ship")
	_, err = msgStore.AppendMessage(ctx, StoredMessage{
		SessionID: sessE, RunID: runE, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "pass the apricot merge"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	p1, err := store.ListSessionsByUser(ctx, userID, "apricot", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Sessions) != 1 || p1.NextCursor == nil {
		t.Fatalf("apricot page 1 = %d sessions, cursor set = %v", len(p1.Sessions), p1.NextCursor != nil)
	}
	got = append(got, titlesOf(p1)...)
	p2, err := store.ListSessionsByUser(ctx, userID, "apricot", 1, p1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Sessions) != 1 || p2.NextCursor != nil {
		t.Fatalf("apricot page 2 = %d sessions, cursor set = %v", len(p2.Sessions), p2.NextCursor != nil)
	}
	got = append(got, titlesOf(p2)...)
	if want := []string{"friday ship", "monday standup"}; !slices.Equal(got, want) {
		t.Fatalf("apricot walk = %v, want %v", got, want)
	}
}

func titlesOf(p SessionPage) []string {
	out := make([]string, 0, len(p.Sessions))
	for _, s := range p.Sessions {
		out = append(out, s.Title)
	}
	return out
}
