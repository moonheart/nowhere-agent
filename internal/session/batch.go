package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SuspendedBatch is the durable identity of a tool batch the interaction gate
// suspended on (capability suspend-batch-snapshot, migration 000019). It is
// written in the same transaction as the batch's first interaction row, so the
// suspension is bound into durable state; a later fold resolves the batch from
// this snapshot — never from a session-wide history scan.
type SuspendedBatch struct {
	RunID      string
	SessionID  string
	MessageSeq *int // seq of the suspending assistant message; nil until known
	// ToolCallIDs is the FULL batch (gated and ungated siblings) in
	// assistant-message block order — the fold's answer key.
	ToolCallIDs []string
	// FoldedSeq is the seq of the folded tool_result message; nil = not folded.
	FoldedSeq *int
	CreatedAt time.Time
}

// ErrNoSuspendedBatch is returned when a fold is requested for a run that has
// no suspended-batch snapshot.
var ErrNoSuspendedBatch = errors.New("no suspended batch for run")

// ErrBatchAlreadyFolded is returned by a fold commit that lost the race to a
// concurrent fold of the same batch. Callers treat it as idempotent success:
// the fold's tool_result message is already persisted.
var ErrBatchAlreadyFolded = errors.New("batch already folded")

// FoldCommitter is the optional store capability for committing a fold — the
// tool_result message plus the folded marker — atomically. PGStore implements
// it; stores that don't (MemStore) use the registry's sequential fallback.
type FoldCommitter interface {
	CommitFold(ctx context.Context, runID string, msg StoredMessage) (StoredMessage, error)
}

// --- MemStore (in-memory, tests/dev) ---

func (m *MemStore) CreateInteractionBatch(_ context.Context, batch SuspendedBatch, in Interaction) (Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.batches[batch.RunID]; !ok {
		batch.CreatedAt = time.Now()
		cp := batch
		m.batches[batch.RunID] = &cp
	}
	return m.createApprovalLocked(in), nil
}

// createApprovalLocked is CreateApproval's body under the store lock, shared by
// CreateApproval and CreateInteractionBatch.
func (m *MemStore) createApprovalLocked(a Interaction) Interaction {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	a.Status = InteractionPending
	a.CreatedAt = time.Now()
	m.approvalSeq++
	a.seq = m.approvalSeq
	cp := a
	m.approvals[a.ID] = &cp
	return a
}

func (m *MemStore) SuspendedBatchForRun(_ context.Context, runID string) (SuspendedBatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[runID]
	if !ok {
		return SuspendedBatch{}, ErrNoSuspendedBatch
	}
	return *b, nil
}

func (m *MemStore) MarkBatchFolded(_ context.Context, runID string, foldedSeq int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[runID]
	if !ok {
		return ErrNoSuspendedBatch
	}
	b.FoldedSeq = &foldedSeq
	return nil
}

// --- PGStore (Postgres, production) ---

func (s *PGStore) CreateInteractionBatch(ctx context.Context, batch SuspendedBatch, in Interaction) (Interaction, error) {
	if len(in.Payload) == 0 {
		in.Payload = json.RawMessage("{}")
	}
	if in.ID == "" {
		in.ID = uuid.NewString()
	}
	kind := in.Kind
	if kind == "" {
		kind = KindToolApproval
	}
	ids, err := json.Marshal(batch.ToolCallIDs)
	if err != nil {
		return Interaction{}, fmt.Errorf("marshal batch tool_call_ids: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Interaction{}, fmt.Errorf("begin interaction batch: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO suspended_batches (run_id, session_id, message_seq, tool_call_ids)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id) DO NOTHING`,
		batch.RunID, batch.SessionID, nullableInt(batch.MessageSeq), ids); err != nil {
		return Interaction{}, fmt.Errorf("insert suspended batch: %w", err)
	}
	row := tx.QueryRowContext(ctx, `
		INSERT INTO approvals (id, run_id, session_id, tool_call_id, tool_name, tool_input, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+approvalCols,
		in.ID, in.RunID, in.SessionID, in.ToolCallID, in.ToolName, in.Payload, kind)
	ap, err := scanApproval(row)
	if err != nil {
		return Interaction{}, fmt.Errorf("create interaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Interaction{}, fmt.Errorf("commit interaction batch: %w", err)
	}
	return ap, nil
}

// suspendedBatchCols is the column list for suspended_batches scans.
const suspendedBatchCols = `run_id, session_id, message_seq, tool_call_ids, folded_seq, created_at`

// scanSuspendedBatch reads one suspended_batches row. message_seq and
// folded_seq are nullable, so they scan through sql.NullInt64 into *int.
func scanSuspendedBatch(row interface{ Scan(...any) error }) (SuspendedBatch, error) {
	var b SuspendedBatch
	var msgSeq, foldedSeq sql.NullInt64
	var ids []byte
	err := row.Scan(&b.RunID, &b.SessionID, &msgSeq, &ids, &foldedSeq, &b.CreatedAt)
	if err != nil {
		return SuspendedBatch{}, err
	}
	if err := json.Unmarshal(ids, &b.ToolCallIDs); err != nil {
		return SuspendedBatch{}, fmt.Errorf("unmarshal batch tool_call_ids: %w", err)
	}
	b.MessageSeq = nullIntPtr(msgSeq)
	b.FoldedSeq = nullIntPtr(foldedSeq)
	return b, nil
}

func (s *PGStore) SuspendedBatchForRun(ctx context.Context, runID string) (SuspendedBatch, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+suspendedBatchCols+`
		FROM suspended_batches WHERE run_id = $1`, runID)
	b, err := scanSuspendedBatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SuspendedBatch{}, ErrNoSuspendedBatch
	}
	if err != nil {
		return SuspendedBatch{}, fmt.Errorf("suspended batch for run: %w", err)
	}
	return b, nil
}

func (s *PGStore) MarkBatchFolded(ctx context.Context, runID string, foldedSeq int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE suspended_batches SET folded_seq = $2 WHERE run_id = $1`,
		runID, foldedSeq)
	if err != nil {
		return fmt.Errorf("mark batch folded: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuspendedBatch
	}
	return nil
}

// CommitFold atomically persists the fold's tool_result message and marks the
// batch folded (PG implementation of FoldCommitter): both writes commit in one
// transaction, so a crash cannot leave a folded message without the marker (or
// vice versa), and a retried fold sees folded_seq set and skips re-execution.
func (s *PGStore) CommitFold(ctx context.Context, runID string, msg StoredMessage) (StoredMessage, error) {
	content, err := json.Marshal(msg.Content)
	if err != nil {
		return StoredMessage{}, fmt.Errorf("marshal content: %w", err)
	}
	if msg.Content == nil {
		content = []byte("[]")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredMessage{}, fmt.Errorf("begin fold commit: %w", err)
	}
	defer tx.Rollback()
	// Claim the fold inside the transaction: a concurrent fold of the same
	// batch (two resume retries racing) must converge to ONE commit, not two
	// tool executions' messages. The row lock serializes the contenders; the
	// loser sees folded_seq set and reports ErrBatchAlreadyFolded.
	var foldedSeq sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT folded_seq FROM suspended_batches WHERE run_id = $1 FOR UPDATE`,
		runID).Scan(&foldedSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredMessage{}, ErrNoSuspendedBatch
	}
	if err != nil {
		return StoredMessage{}, fmt.Errorf("lock suspended batch: %w", err)
	}
	if foldedSeq.Valid {
		return StoredMessage{}, ErrBatchAlreadyFolded
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO messages (session_id, run_id, seq, role, content)
		VALUES ($1, $2,
			(SELECT COALESCE(MAX(seq), -1) + 1 FROM messages WHERE session_id = $1),
			$3, $4)
		RETURNING id, seq, created_at`,
		msg.SessionID, msg.RunID, string(msg.Role), content,
	).Scan(&msg.ID, &msg.Seq, &msg.CreatedAt)
	if err != nil {
		return StoredMessage{}, fmt.Errorf("append fold message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE suspended_batches SET folded_seq = $2 WHERE run_id = $1`,
		runID, msg.Seq); err != nil {
		return StoredMessage{}, fmt.Errorf("mark batch folded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StoredMessage{}, fmt.Errorf("commit fold: %w", err)
	}
	return msg, nil
}

// nullableInt maps a nil seq pointer to SQL NULL.
func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullIntPtr maps a nullable SQL integer to a *int (nil when NULL).
func nullIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
