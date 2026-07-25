package dreaming

import (
	"context"
	"testing"

	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// TestStoreSourceOverSessionStores pins the production EpisodeSource wiring
// (capability-gap K1): eligibility comes from ListEndedUndreamed, episodes from
// MessagesFor, and MarkProcessed stamps the session dreamed so it drops out of
// the next scan.
func TestStoreSourceOverSessionStores(t *testing.T) {
	ctx := context.Background()
	sessions := session.NewMemStore()
	messages := session.NewMemMessageStore()
	src := NewStoreSource(sessions, messages)

	ended, _ := sessions.CreateSession(ctx, "user1", "ended")
	active, _ := sessions.CreateSession(ctx, "user1", "active")
	if err := sessions.EndSession(ctx, ended.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := messages.AppendMessage(ctx, session.StoredMessage{
		SessionID: ended.ID,
		Role:      provider.RoleUser,
		Content:   []provider.Block{{Type: provider.BlockText, Text: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Only the ended session is eligible, carrying its user scope.
	list, err := src.EndedSessions(ctx)
	if err != nil {
		t.Fatalf("EndedSessions: %v", err)
	}
	if len(list) != 1 || list[0].ID != ended.ID || list[0].UserID != "user1" {
		t.Fatalf("EndedSessions = %+v, want only the ended session for user1", list)
	}
	for _, s := range list {
		if s.ID == active.ID {
			t.Errorf("active session %s must not be eligible", active.ID)
		}
	}

	// Episodes are the session's persisted messages.
	eps, err := src.Episodes(ctx, ended.ID)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 1 || eps[0].Content[0].Text != "hi" {
		t.Fatalf("Episodes = %+v", eps)
	}

	// Marking processed removes the session from the next scan.
	if err := src.MarkProcessed(ctx, ended.ID); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	list, _ = src.EndedSessions(ctx)
	if len(list) != 0 {
		t.Errorf("after MarkProcessed, want no eligible sessions, got %+v", list)
	}
}

// TestWorkerOverStoreSource runs the full worker against the real StoreSource
// over in-memory stores: an ended session's episodes become user-scoped facts
// and the session is stamped dreamed (so a second Run is a no-op).
func TestWorkerOverStoreSource(t *testing.T) {
	ctx := context.Background()
	sessions := session.NewMemStore()
	messages := session.NewMemMessageStore()
	src := NewStoreSource(sessions, messages)

	ended, _ := sessions.CreateSession(ctx, "user9", "x")
	_ = sessions.EndSession(ctx, ended.ID)
	_, _ = messages.AppendMessage(ctx, session.StoredMessage{
		SessionID: ended.ID,
		Role:      provider.RoleUser,
		Content:   []provider.Block{{Type: provider.BlockText, Text: "user likes go"}},
	})

	mem := memory.NewMemPort()
	llm := &fakeLLM{output: "- user likes go", tokens: 40}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesWritten != 1 {
		t.Errorf("memories written = %d want 1", res.MemoriesWritten)
	}

	// Second run: nothing left to dream over.
	res2, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.EpisodesProcessed != 0 || res2.MemoriesWritten != 0 {
		t.Errorf("second run should be a no-op, got %+v", res2)
	}
}
