package session

import (
	"context"
	"testing"
)

// TestMemStoreDreamedSeq pins the in-memory watermark: it starts at 0, advances
// monotonically, and never moves backwards.
func TestMemStoreDreamedSeq(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	s, _ := m.CreateSession(ctx, "u1", "a")

	if got, _ := m.DreamedSeq(ctx, s.ID); got != 0 {
		t.Fatalf("initial dreamed_seq = %d want 0", got)
	}

	if err := m.MarkDreamedSeq(ctx, s.ID, 5); err != nil {
		t.Fatalf("MarkDreamedSeq: %v", err)
	}
	if got, _ := m.DreamedSeq(ctx, s.ID); got != 5 {
		t.Errorf("dreamed_seq = %d want 5", got)
	}

	// Never backwards.
	if err := m.MarkDreamedSeq(ctx, s.ID, 3); err != nil {
		t.Fatalf("MarkDreamedSeq backwards: %v", err)
	}
	if got, _ := m.DreamedSeq(ctx, s.ID); got != 5 {
		t.Errorf("dreamed_seq regressed to %d, want it to stay 5", got)
	}

	// Unknown session reads as 0.
	if got, _ := m.DreamedSeq(ctx, "nope"); got != 0 {
		t.Errorf("unknown session dreamed_seq = %d want 0", got)
	}
}

// TestPGStoreIncrementalDreaming exercises the watermark model against
// Postgres: a session with new messages is eligible (regardless of status),
// MarkDreamedSeq advances the watermark, and MessagesAfter returns only the
// tail. Skips when no database is reachable.
func TestPGStoreIncrementalDreaming(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	messages := NewPGMessageStore(db)
	ctx := context.Background()
	userID := pgNewUser(t, db)

	sess, err := store.CreateSession(ctx, userID, "incr")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Hard-delete the session (cascades to the run + messages) so its run doesn't
	// linger active for later tests.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID)
	})

	// Fresh session has no messages → not eligible (even before any watermark).
	if eligible, err := store.ListUndreamedSessions(ctx); err != nil {
		t.Fatalf("ListUndreamedSessions: %v", err)
	} else {
		for _, s := range eligible {
			if s.ID == sess.ID {
				t.Errorf("session with no messages must not be eligible")
			}
		}
	}

	appendTwo := func() (first, second StoredMessage) {
		first, err = messages.AppendMessage(ctx, StoredMessage{SessionID: sess.ID, RunID: run.ID, Role: "user"})
		if err != nil {
			t.Fatal(err)
		}
		second, err = messages.AppendMessage(ctx, StoredMessage{SessionID: sess.ID, RunID: run.ID, Role: "assistant"})
		if err != nil {
			t.Fatal(err)
		}
		return first, second
	}
	m1, m2 := appendTwo()

	contains := func(list []Session, id string) bool {
		for _, s := range list {
			if s.ID == id {
				return true
			}
		}
		return false
	}

	// Now it has undreamed messages → eligible (status is still active).
	eligible, err := store.ListUndreamedSessions(ctx)
	if err != nil {
		t.Fatalf("ListUndreamedSessions: %v", err)
	}
	if !contains(eligible, sess.ID) {
		t.Errorf("active session with new messages should be eligible")
	}

	// MessagesAfter the watermark returns both; after m1 only the second.
	msgs, err := messages.MessagesAfter(ctx, sess.ID, 0)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("MessagesAfter(0) = %d err %v want 2", len(msgs), err)
	}
	msgs, err = messages.MessagesAfter(ctx, sess.ID, m1.ID)
	if err != nil || len(msgs) != 1 || msgs[0].ID != m2.ID {
		t.Fatalf("MessagesAfter(m1) = %+v err %v want only m2", msgs, err)
	}

	// Advance the watermark to m2 → session drops out of eligibility.
	if err := store.MarkDreamedSeq(ctx, sess.ID, m2.ID); err != nil {
		t.Fatalf("MarkDreamedSeq: %v", err)
	}
	if seq, _ := store.DreamedSeq(ctx, sess.ID); seq != m2.ID {
		t.Errorf("dreamed_seq = %d want %d", seq, m2.ID)
	}
	eligible, _ = store.ListUndreamedSessions(ctx)
	if contains(eligible, sess.ID) {
		t.Errorf("fully-dreamed session should drop out of eligibility")
	}

	// A new message re-qualifies it, resumed after the watermark.
	m3, err := messages.AppendMessage(ctx, StoredMessage{SessionID: sess.ID, RunID: run.ID, Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	eligible, _ = store.ListUndreamedSessions(ctx)
	if !contains(eligible, sess.ID) {
		t.Errorf("session with a newer message should be eligible again")
	}
	msgs, _ = messages.MessagesAfter(ctx, sess.ID, m2.ID)
	if len(msgs) != 1 || msgs[0].ID != m3.ID {
		t.Errorf("MessagesAfter(m2) = %+v want only m3", msgs)
	}
}
