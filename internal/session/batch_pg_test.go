package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
)

// TestPGInteractionBatchAtomicAndIdempotent pins the snapshot write path: the
// batch row commits with the first interaction, and a second gated call of the
// same run adds its interaction WITHOUT rewriting the snapshot.
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

// TestPGSweepExpiredInteractions marks stale pendings expired (releasing the
// session's pending gate) while fresh/decided rows survive, and a stale verdict
// on an expired row is refused.
func TestPGSweepExpiredInteractions(t *testing.T) {
	db := pgTestDB(t)
	store := NewPGStore(db)
	ctx := context.Background()
	sessID, runID := setupMessageSession(t, ctx, db, store)

	old, err := store.CreateApproval(ctx, Interaction{RunID: runID, SessionID: sessID, ToolCallID: "old", ToolName: "danger"})
	if err != nil {
		t.Fatalf("create old: %v", err)
	}
	// Backdate ONLY the row this test created, so the sweep's WHERE clause can
	// never touch sibling tests' rows.
	if _, err := db.Exec(`UPDATE approvals SET created_at = now() - interval '48 hours' WHERE id = $1`, old.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	fresh, err := store.CreateApproval(ctx, Interaction{RunID: runID, SessionID: sessID, ToolCallID: "fresh", ToolName: "danger"})
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	n, err := store.SweepExpiredInteractions(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 1 {
		t.Fatalf("swept = %d, want at least the backdated pending", n)
	}
	// The session's pending gate now reads empty.
	pending, err := store.PendingApprovalsForSession(ctx, sessID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after sweep = %+v err %v, want only the fresh one", pending, err)
	}
	if pending[0].ID != fresh.ID {
		t.Errorf("pending after sweep = %+v, want the fresh interaction", pending[0])
	}
	got, err := store.GetApproval(ctx, old.ID)
	if err != nil || got.Status != InteractionExpired {
		t.Fatalf("swept row = %+v err %v, want expired", got, err)
	}
	if got.DecidedAt != nil {
		t.Error("expiry must not set decided_at (the row was never decided)")
	}
	// A stale verdict is refused: the row is no longer pending.
	if _, err := store.DecideApproval(ctx, old.ID, true, nil); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("decide expired = %v, want ErrNoPendingApproval", err)
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

// TestPGCommitFoldConcurrentLoser: a second fold commit for an already-folded
// batch reports ErrBatchAlreadyFolded (idempotent-success sentinel) and writes
// no duplicate message — two racing resume retries converge to ONE commit.
func TestPGCommitFoldConcurrentLoser(t *testing.T) {
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
	foldMsg := StoredMessage{
		SessionID: sessID, RunID: runID, Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockToolResult, ToolResultID: "tu1", ToolContent: "done"}},
	}
	if _, err := store.CommitFold(ctx, runID, foldMsg); err != nil {
		t.Fatalf("first CommitFold: %v", err)
	}
	if _, err := store.CommitFold(ctx, runID, foldMsg); !errors.Is(err, ErrBatchAlreadyFolded) {
		t.Fatalf("second CommitFold = %v, want ErrBatchAlreadyFolded", err)
	}
	msgs, err := ms.MessagesFor(ctx, sessID)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("MessagesFor = %+v err %v, want exactly one fold message (no duplicate)", msgs, err)
	}
}
