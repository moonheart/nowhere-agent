package dreaming

import (
	"context"
	"testing"

	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// appendMsg is a test helper that appends a user text message.
func appendMsg(t *testing.T, ms *session.MemMessageStore, sessionID, text string) session.StoredMessage {
	t.Helper()
	m, err := ms.AppendMessage(context.Background(), session.StoredMessage{
		SessionID: sessionID,
		Role:      provider.RoleUser,
		Content:   []provider.Block{{Type: provider.BlockText, Text: text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestStoreSourceIncrementalOverMemStores pins the watermark model over the
// in-memory stores (the fallback eligibility path): an OPEN session with
// messages is eligible; a pass advances the watermark; new messages re-qualify
// the session; only the un-dreamed tail is returned each time.
func TestStoreSourceIncrementalOverMemStores(t *testing.T) {
	ctx := context.Background()
	sessions := session.NewMemStore()
	messages := session.NewMemMessageStore()
	src := NewStoreSource(sessions, messages)

	sess, err := sessions.CreateSession(ctx, "user1", "chat")
	if err != nil {
		t.Fatal(err)
	}
	// NOTE: the session stays ACTIVE the whole time — learning must not require ending.

	m1 := appendMsg(t, messages, sess.ID, "first")
	m2 := appendMsg(t, messages, sess.ID, "second")

	// Eligible (it has undreamed messages), watermark 0.
	pending, err := src.PendingSessions(ctx)
	if err != nil {
		t.Fatalf("PendingSessions: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != sess.ID || pending[0].UserID != "user1" || pending[0].Seq != 0 {
		t.Fatalf("PendingSessions = %+v, want the active session at watermark 0", pending)
	}

	// Episodes from the watermark return both messages.
	eps, err := src.Episodes(ctx, sess.ID, pending[0].Seq)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("Episodes = %d want 2", len(eps))
	}

	// Advance the watermark to the newest consumed message.
	if err := src.MarkProcessed(ctx, sess.ID, eps[len(eps)-1].ID); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	// Now nothing is pending.
	if pending, _ := src.PendingSessions(ctx); len(pending) != 0 {
		t.Errorf("after MarkProcessed, want no pending sessions, got %+v", pending)
	}

	// A new message re-qualifies the session, resumed after the watermark.
	m3 := appendMsg(t, messages, sess.ID, "third")
	pending, _ = src.PendingSessions(ctx)
	if len(pending) != 1 || pending[0].Seq != m2.ID {
		t.Fatalf("after a new message, want the session pending at watermark %d, got %+v", m2.ID, pending)
	}
	eps, _ = src.Episodes(ctx, sess.ID, pending[0].Seq)
	if len(eps) != 1 || eps[0].ID != m3.ID || eps[0].Content[0].Text != "third" {
		t.Errorf("incremental Episodes = %+v, want only the new message", eps)
	}
	_ = m1
}

// TestWorkerIncrementalOverStores runs the full worker against the real
// StoreSource over in-memory stores: a pass consolidates the tail, a second
// pass with no new messages is a no-op, and a later message is learned next.
func TestWorkerIncrementalOverStores(t *testing.T) {
	ctx := context.Background()
	sessions := session.NewMemStore()
	messages := session.NewMemMessageStore()
	src := NewStoreSource(sessions, messages)

	sess, _ := sessions.CreateSession(ctx, "user9", "x")
	appendMsg(t, messages, sess.ID, "user likes go")

	mem := memory.NewMemPort()
	// extract → 1 fact; compress → a summary; reflect → nothing new.
	llm := &fakeLLM{outputs: []string{"- user likes go", "likes go", ""}, tokens: 40}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesWritten != 2 {
		t.Errorf("memories written = %d want 2 (1 fact + 1 summary)", res.MemoriesWritten)
	}

	// No new messages → second pass is a no-op.
	res2, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.EpisodesProcessed != 0 || res2.MemoriesWritten != 0 {
		t.Errorf("second pass should be a no-op, got %+v", res2)
	}
	if llm.calls != 3 {
		t.Errorf("llm calls = %d want 3 (extract+compress+reflect; no work on the no-op pass)", llm.calls)
	}

	// A later message is picked up on the next pass.
	appendMsg(t, messages, sess.ID, "user prefers dark mode")
	res3, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res3.EpisodesProcessed != 1 {
		t.Errorf("third pass episodes = %d want 1 (only the new message)", res3.EpisodesProcessed)
	}
}
