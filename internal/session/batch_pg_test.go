package session

import (
	"context"
	"errors"
	"testing"

	"nowhere-agent/internal/provider"
)

// TestPGInteractionBatchAtomicAndIdempotent pins the snapshot write path: the
// batch row commits with the first interaction, and a second gated call of the
// same run adds its interaction WITHOUT rewriting the snapshot.
func TestPGInteractionBatchAtomicAndIdempotent(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	sessID, runID := setupMessageSession(t, ctx, db, store)

	batch := SuspendedBatch{RunID: runID, SessionID: sessID, ToolCallIDs: []string{"tu1", "tu2"}}
	ap1, err := store.CreateInteractionBatch(ctx, batch, Interaction{
		RunID: runID, SessionID: sessID, ToolCallID: "tu1", ToolName: "danger", Kind: KindToolApproval,
	})
	if err != nil {
		t.Fatalf("CreateInteractionBatch 1: %v", err)
	}
	ap2, err := store.CreateInteractionBatch(ctx, SuspendedBatch{RunID: runID, SessionID: sessID, ToolCallIDs: []string{"CHANGED"}}, Interaction{
		RunID: runID, SessionID: sessID, ToolCallID: "tu2", ToolName: "danger2", Kind: KindToolApproval,
	})
	if err != nil {
		t.Fatalf("CreateInteractionBatch 2: %v", err)
	}
	if ap1.ID == ap2.ID {
		t.Fatal("two interactions got the same id")
	}

	got, err := store.SuspendedBatchForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SuspendedBatchForRun: %v", err)
	}
	if len(got.ToolCallIDs) != 2 || got.ToolCallIDs[0] != "tu1" || got.ToolCallIDs[1] != "tu2" {
		t.Errorf("snapshot IDs = %v, want the first insert's [tu1 tu2] (idempotent)", got.ToolCallIDs)
	}
	if got.FoldedSeq != nil {
		t.Errorf("FoldedSeq = %v, want nil before the fold", *got.FoldedSeq)
	}
	pending, err := store.PendingApprovalsForRun(ctx, runID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending interactions = %+v err %v, want 2", pending, err)
	}
}

// TestPGSuspendedBatchForRunMissing: a run without a snapshot errors, never
// silently folds.
func TestPGSuspendedBatchForRunMissing(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	_, runID := setupMessageSession(t, ctx, db, store)

	if _, err := store.SuspendedBatchForRun(ctx, runID); !errors.Is(err, ErrNoSuspendedBatch) {
		t.Fatalf("SuspendedBatchForRun = %v, want ErrNoSuspendedBatch", err)
	}
	if err := store.MarkBatchFolded(ctx, runID, 3); !errors.Is(err, ErrNoSuspendedBatch) {
		t.Fatalf("MarkBatchFolded = %v, want ErrNoSuspendedBatch", err)
	}
}

// TestPGCommitFoldAtomic: the fold's tool_result message and the folded marker
// land together — the message gets a seq and the batch records THAT seq.
func TestPGCommitFoldAtomic(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ms := NewPGMessageStore(db)
	ctx := context.Background()
	sessID, runID := setupMessageSession(t, ctx, db, store)

	if _, err := store.CreateInteractionBatch(ctx, SuspendedBatch{RunID: runID, SessionID: sessID, ToolCallIDs: []string{"tu1"}}, Interaction{
		RunID: runID, SessionID: sessID, ToolCallID: "tu1", ToolName: "danger", Kind: KindToolApproval,
	}); err != nil {
		t.Fatalf("CreateInteractionBatch: %v", err)
	}

	committed, err := store.CommitFold(ctx, runID, StoredMessage{
		SessionID: sessID, RunID: runID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockToolResult, ToolResultID: "tu1", ToolContent: "done"}},
	})
	if err != nil {
		t.Fatalf("CommitFold: %v", err)
	}

	batch, err := store.SuspendedBatchForRun(ctx, runID)
	if err != nil {
		t.Fatalf("SuspendedBatchForRun: %v", err)
	}
	if batch.FoldedSeq == nil || *batch.FoldedSeq != committed.Seq {
		t.Errorf("FoldedSeq = %v, want the committed message seq %d", batch.FoldedSeq, committed.Seq)
	}
	msgs, err := ms.MessagesFor(ctx, sessID)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("MessagesFor = %+v err %v, want the fold message", msgs, err)
	}
	if msgs[0].Content[0].ToolResultID != "tu1" {
		t.Errorf("folded message = %+v", msgs[0])
	}

	// A commit against a run with no snapshot rolls the whole transaction back:
	// the error is ErrNoSuspendedBatch and the message is NOT persisted. (The
	// first run must settle before a second can exist — one active run per
	// session.)
	if err := store.UpdateRunStatus(ctx, runID, RunDone); err != nil {
		t.Fatalf("settle run1: %v", err)
	}
	run2, err := store.CreateRun(ctx, sessID, 2)
	if err != nil {
		t.Fatalf("create run2: %v", err)
	}
	if _, err := store.CommitFold(ctx, run2.ID, StoredMessage{
		SessionID: sessID, RunID: run2.ID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockToolResult, ToolResultID: "tuX", ToolContent: "x"}},
	}); !errors.Is(err, ErrNoSuspendedBatch) {
		t.Fatalf("CommitFold for batchless run = %v, want ErrNoSuspendedBatch", err)
	}
	msgs, err = ms.MessagesFor(ctx, sessID)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("after failed commit MessagesFor = %+v err %v, want only the first fold message", msgs, err)
	}
}
