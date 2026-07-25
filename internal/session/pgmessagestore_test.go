package session

import (
	"context"
	"database/sql"
	"testing"

	"nowhere-agent/internal/provider"
)

// setupMessageSession creates a session + run to attach messages to, and
// returns their ids. Cleanup HARD-deletes the session (cascading to runs and
// messages): a soft-delete (status='ended') would leave the run 'queued', and
// the partial unique index idx_runs_one_active_per_session would then make a
// later test's run on a *different* session collide if rows accumulate — and
// leave the shared dev DB dirty.
func setupMessageSession(t *testing.T, ctx context.Context, db *sql.DB, s *PGStore) (sessionID, runID string) {
	t.Helper()
	userID := pgNewUser(t, db)
	sess, err := s.CreateSession(ctx, userID, "msg-test-"+randSuffix())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	run, err := s.CreateRun(ctx, sess.ID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID)
	})
	return sess.ID, run.ID
}

func TestPGMessageStoreAppendAndOrder(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ms := NewPGMessageStore(db)
	ctx := context.Background()
	sessID, runID := setupMessageSession(t, ctx, db, store)

	for i, text := range []string{"first", "second", "third"} {
		if _, err := ms.AppendMessage(ctx, StoredMessage{
			SessionID: sessID,
			RunID:     runID,
			Role:      provider.RoleUser,
			Content:   []provider.Block{{Type: provider.BlockText, Text: text}},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	msgs, err := ms.MessagesFor(ctx, sessID)
	if err != nil {
		t.Fatalf("MessagesFor: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	for i, want := range []string{"first", "second", "third"} {
		if msgs[i].Seq != i {
			t.Errorf("msg %d seq = %d, want %d", i, msgs[i].Seq, i)
		}
		if msgs[i].Content[0].Text != want {
			t.Errorf("msg %d text = %q, want %q", i, msgs[i].Content[0].Text, want)
		}
		if msgs[i].Role != provider.RoleUser {
			t.Errorf("msg %d role = %q", i, msgs[i].Role)
		}
	}
}

func TestPGMessageStoreFullBlockRoundTrip(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ms := NewPGMessageStore(db)
	ctx := context.Background()
	sessID, runID := setupMessageSession(t, ctx, db, store)

	// An assistant message with thinking(+signature), text, and a tool_use,
	// followed by a user tool_result — the shapes compression must see.
	assistant := StoredMessage{
		SessionID: sessID,
		RunID:     runID,
		Role:      provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockThinking, Thinking: "hmm", ThinkingSignature: "sig-1"},
			{Type: provider.BlockText, Text: "let me read that"},
			{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "read", ToolInput: map[string]any{"path": "/x"}},
		},
	}
	toolResult := StoredMessage{
		SessionID: sessID,
		RunID:     runID,
		Role:      provider.RoleUser,
		Content: []provider.Block{
			{Type: provider.BlockToolResult, ToolResultID: "tu1", ToolContent: "file body", IsError: false},
		},
	}
	if _, err := ms.AppendMessage(ctx, assistant); err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	if _, err := ms.AppendMessage(ctx, toolResult); err != nil {
		t.Fatalf("append tool result: %v", err)
	}

	msgs, err := ms.MessagesFor(ctx, sessID)
	if err != nil {
		t.Fatalf("MessagesFor: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	th := msgs[0].Content[0]
	if th.Type != provider.BlockThinking || th.Thinking != "hmm" || th.ThinkingSignature != "sig-1" {
		t.Errorf("thinking block wrong: %+v", th)
	}
	tu := msgs[0].Content[2]
	if tu.Type != provider.BlockToolUse || tu.ToolUseID != "tu1" || tu.ToolInput["path"] != "/x" {
		t.Errorf("tool_use block wrong: %+v", tu)
	}
	tr := msgs[1].Content[0]
	if tr.Type != provider.BlockToolResult || tr.ToolResultID != "tu1" || tr.ToolContent != "file body" || tr.IsError {
		t.Errorf("tool_result block wrong: %+v", tr)
	}
}

func TestPGMessageStoreSeqContinuesAcrossRuns(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ms := NewPGMessageStore(db)
	ctx := context.Background()
	sessID, run1 := setupMessageSession(t, ctx, db, store)

	if _, err := ms.AppendMessage(ctx, StoredMessage{SessionID: sessID, RunID: run1, Role: provider.RoleUser, Content: []provider.Block{{Type: provider.BlockText, Text: "run1"}}}); err != nil {
		t.Fatalf("run1 append: %v", err)
	}

	// A second run on the same session must continue the session seq, not reset.
	// run1 is still queued (active), and the single-active-run constraint
	// (idx_runs_one_active_per_session, migration 000007) forbids a second active
	// run in the session — so settle run1 first, as a real turn would.
	if err := store.UpdateRunStatus(ctx, run1, RunDone); err != nil {
		t.Fatalf("settle run1: %v", err)
	}
	run2, err := store.CreateRun(ctx, sessID, 2)
	if err != nil {
		t.Fatalf("create run2: %v", err)
	}
	m, err := ms.AppendMessage(ctx, StoredMessage{SessionID: sessID, RunID: run2.ID, Role: provider.RoleUser, Content: []provider.Block{{Type: provider.BlockText, Text: "run2"}}})
	if err != nil {
		t.Fatalf("run2 append: %v", err)
	}
	if m.Seq != 1 {
		t.Errorf("run2 first message seq = %d, want 1 (continued from run1)", m.Seq)
	}
}

func TestMemMessageStoreSeqAndOrder(t *testing.T) {
	ms := NewMemMessageStore()
	ctx := context.Background()
	for i, text := range []string{"a", "b"} {
		m, err := ms.AppendMessage(ctx, StoredMessage{SessionID: "s1", RunID: "r1", Role: provider.RoleUser, Content: []provider.Block{{Type: provider.BlockText, Text: text}}})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if m.Seq != i {
			t.Errorf("seq = %d, want %d", m.Seq, i)
		}
	}
	msgs, err := ms.MessagesFor(ctx, "s1")
	if err != nil {
		t.Fatalf("MessagesFor: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content[0].Text != "a" || msgs[1].Content[0].Text != "b" {
		t.Errorf("unexpected messages: %+v", msgs)
	}
}
