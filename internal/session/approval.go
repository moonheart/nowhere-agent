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

// ApprovalStatus is the lifecycle of a pending tool-approval decision.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	ApprovalExpired  ApprovalStatus = "expired"
)

// Approval is the durable record of one human interaction a run is parked on
// (capability-gap O2 + ask_user, migration 000010). Two kinds: a permission
// approval (a dangerous call needing yes/no) and an ask_user question set (the
// model asking structured input). Both suspend the run (RunWaitingApproval);
// the decision endpoint resolves the row and resumes, so the pause survives
// process restarts.
type Approval struct {
	ID         string
	RunID      string
	SessionID  string
	ToolCallID string
	ToolName   string
	// ToolInput is the model-supplied arguments: for an approval, the gated
	// call's args (re-executed on approve); for ask_user, the question set shown
	// to the user.
	ToolInput json.RawMessage
	// Kind is "approval" or "ask_user" (empty = approval).
	Kind   string
	Status ApprovalStatus
	// Answer is the user's structured response for ask_user (e.g. {"answers":{...}}).
	Answer    json.RawMessage
	CreatedAt time.Time
	DecidedAt *time.Time
}

// ErrNoPendingApproval is returned when resolving/fetching an approval that is
// not pending (unknown id, or already decided).
var ErrNoPendingApproval = errors.New("no pending approval")

// --- MemStore (in-memory, tests/dev) ---

func (m *MemStore) CreateApproval(_ context.Context, a Approval) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.approvals {
		if ex.SessionID == a.SessionID && ex.Status == ApprovalPending {
			return Approval{}, fmt.Errorf("session %s already has a pending approval", a.SessionID)
		}
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	a.Status = ApprovalPending
	a.CreatedAt = time.Now()
	cp := a
	m.approvals[a.ID] = &cp
	return a, nil
}

func (m *MemStore) PendingApprovalForSession(_ context.Context, sessionID string) (Approval, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.approvals {
		if a.SessionID == sessionID && a.Status == ApprovalPending {
			return *a, true, nil
		}
	}
	return Approval{}, false, nil
}

func (m *MemStore) GetApproval(_ context.Context, id string) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, ErrNoPendingApproval
	}
	return *a, nil
}

func (m *MemStore) DecideApproval(_ context.Context, id string, approve bool, answer json.RawMessage) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok || a.Status != ApprovalPending {
		return Approval{}, ErrNoPendingApproval
	}
	now := time.Now()
	a.DecidedAt = &now
	a.Answer = answer
	if approve {
		a.Status = ApprovalApproved
	} else {
		a.Status = ApprovalRejected
	}
	return *a, nil
}

// --- PGStore (Postgres, production) ---

// approvalCols is the shared column list for approvals scans (kind + answer
// included so every read returns the full record).
const approvalCols = `id, run_id, session_id, tool_call_id, tool_name, tool_input, kind, status, answer, created_at, decided_at`

// scanApproval reads one approvals row. answer is nullable in the DB, so it
// scans through a []byte (nil-safe) rather than json.RawMessage, which the
// driver cannot store a NULL into.
func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var answer []byte
	err := row.Scan(&a.ID, &a.RunID, &a.SessionID, &a.ToolCallID, &a.ToolName,
		&a.ToolInput, &a.Kind, &a.Status, &answer, &a.CreatedAt, &a.DecidedAt)
	a.Answer = answer
	return a, err
}

func (s *PGStore) CreateApproval(ctx context.Context, a Approval) (Approval, error) {
	if len(a.ToolInput) == 0 {
		a.ToolInput = json.RawMessage("{}")
	}
	kind := a.Kind
	if kind == "" {
		kind = "approval"
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO approvals (run_id, session_id, tool_call_id, tool_name, tool_input, kind)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+approvalCols,
		a.RunID, a.SessionID, a.ToolCallID, a.ToolName, a.ToolInput, kind)
	ap, err := scanApproval(row)
	if err != nil {
		return Approval{}, fmt.Errorf("create approval: %w", err)
	}
	return ap, nil
}

func (s *PGStore) PendingApprovalForSession(ctx context.Context, sessionID string) (Approval, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+approvalCols+`
		FROM approvals
		WHERE session_id = $1 AND status = 'pending'
		ORDER BY created_at DESC LIMIT 1`, sessionID)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("pending approval for session: %w", err)
	}
	return a, true, nil
}

func (s *PGStore) GetApproval(ctx context.Context, id string) (Approval, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+approvalCols+`
		FROM approvals WHERE id = $1`, id)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNoPendingApproval
	}
	if err != nil {
		return Approval{}, fmt.Errorf("get approval: %w", err)
	}
	return a, nil
}

// DecideApproval atomically resolves a pending approval (optimistic: the
// status='pending' predicate makes a concurrent double-decide a no-op → error).
// answer is the user's structured response for ask_user (nil for approvals).
func (s *PGStore) DecideApproval(ctx context.Context, id string, approve bool, answer json.RawMessage) (Approval, error) {
	status := string(ApprovalRejected)
	if approve {
		status = string(ApprovalApproved)
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE approvals SET status = $2, answer = $3, decided_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+approvalCols,
		id, status, nullableJSON(answer))
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNoPendingApproval
	}
	if err != nil {
		return Approval{}, fmt.Errorf("decide approval: %w", err)
	}
	return a, nil
}

// nullableJSON maps an empty answer to SQL NULL (the answer column is nullable).
func nullableJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return j
}
