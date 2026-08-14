package session

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
)

// TestPGSweepEndedConversations pins the conversation retention sweep: only
// ENDED sessions whose ended_at predates the cutoff are hard-deleted (with
// their cascaded rows); a freshly-ended session inside the window and an
// active session survive. Re-running the sweep removes nothing (idempotent).
func TestPGSweepEndedConversations(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ms := NewPGMessageStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	oldSess, err := store.CreateSession(ctx, userID, "old")
	if err != nil {
		t.Fatalf("create old session: %v", err)
	}
	freshSess, err := store.CreateSession(ctx, userID, "fresh")
	if err != nil {
		t.Fatalf("create fresh session: %v", err)
	}
	activeSess, err := store.CreateSession(ctx, userID, "active")
	if err != nil {
		t.Fatalf("create active session: %v", err)
	}
	if err := store.EndSession(ctx, oldSess.ID); err != nil {
		t.Fatalf("end old session: %v", err)
	}
	if err := store.EndSession(ctx, freshSess.ID); err != nil {
		t.Fatalf("end fresh session: %v", err)
	}
	// Age the old session's ended_at past the cutoff; the fresh one stays
	// inside the retention window.
	if _, err := db.Exec(`UPDATE sessions SET ended_at = now() - interval '40 days' WHERE id = $1`, oldSess.ID); err != nil {
		t.Fatal(err)
	}

	// Give the old session a run and a message so the test also pins the FK
	// cascade: deleting the session must remove its durable rows.
	run, err := store.CreateRun(ctx, oldSess.ID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := ms.AppendMessage(ctx, StoredMessage{
		SessionID: oldSess.ID,
		RunID:     run.ID,
		Role:      provider.RoleUser,
		Content:   []provider.Block{{Type: provider.BlockText, Text: "hi"}},
	}); err != nil {
		t.Fatalf("append message: %v", err)
	}

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	var cleaned []string
	removed, err := SweepEndedConversations(ctx, nil, store, cutoff, 10, func(id string) { cleaned = append(cleaned, id) })
	if err != nil {
		t.Fatalf("SweepEndedConversations: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the aged session)", removed)
	}
	if len(cleaned) != 1 || cleaned[0] != oldSess.ID {
		t.Errorf("cleanup hook got %v, want [%s]", cleaned, oldSess.ID)
	}

	if _, err := store.GetSession(ctx, oldSess.ID); err == nil {
		t.Errorf("aged session %s must be deleted", oldSess.ID)
	}
	if _, err := store.GetSession(ctx, freshSess.ID); err != nil {
		t.Errorf("freshly-ended session %s must survive: %v", freshSess.ID, err)
	}
	if _, err := store.GetSession(ctx, activeSess.ID); err != nil {
		t.Errorf("active session %s must survive: %v", activeSess.ID, err)
	}
	// The cascade must have removed the old session's message rows.
	if msgs, err := ms.MessagesFor(ctx, oldSess.ID); err != nil {
		t.Fatalf("MessagesFor deleted session: %v", err)
	} else if len(msgs) != 0 {
		t.Errorf("messages of deleted session survive: %d rows", len(msgs))
	}

	// Idempotent: a second pass over the same cutoff removes nothing.
	if again, err := SweepEndedConversations(ctx, nil, store, cutoff, 10, nil); err != nil {
		t.Fatalf("second sweep: %v", err)
	} else if again != 0 {
		t.Errorf("second sweep removed %d, want 0", again)
	}
}
