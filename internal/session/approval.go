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

// Approval is the durable record of one dangerous tool call awaiting a human
// verdict (capability-gap O2, migration 000010). When the loop hits an Ask
// permission verdict it does NOT execute the call; it persists an Approval and
// suspends the run (RunWaitingApproval). The decision endpoint resolves the row
// and resumes the run, so the pause survives process restarts.
type Approval struct {
	ID         string
	RunID      string
	SessionID  string
	ToolCallID string
	ToolName   string
	// ToolInput is the model-supplied arguments, kept so Resume can execute the
	// approved call exactly as requested (or show the user what was asked).
	ToolInput  json.RawMessage
	Status     ApprovalStatus
	CreatedAt  time.Time
	DecidedAt  *time.Time
}

// ErrNoPendingApproval is returned when resolving/fetching an approval that is
// not pending (unknown id, or already decided).
var ErrNoPendingApproval = errors.New("no pending approval")

// --- MemStore (in-memory, tests/dev) ---

func (m *MemStore) CreateApproval(_ context.Context, a Approval) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.approvals {
		if ex.RunID == a.RunID && ex.Status == ApprovalPending {
			return Approval{}, fmt.Errorf("run %s already has a pending approval", a.RunID)
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

func (m *MemStore) PendingApprovalForRun(_ context.Context, runID string) (Approval, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.approvals {
		if a.RunID == runID && a.Status == ApprovalPending {
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

func (m *MemStore) DecideApproval(_ context.Context, id string, approve bool) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok || a.Status != ApprovalPending {
		return Approval{}, ErrNoPendingApproval
	}
	now := time.Now()
	a.DecidedAt = &now
	if approve {
		a.Status = ApprovalApproved
	} else {
		a.Status = ApprovalRejected
	}
	return *a, nil
}

// --- PGStore (Postgres, production) ---

// scanApproval reads one approvals row.
func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	err := row.Scan(&a.ID, &a.RunID, &a.SessionID, &a.ToolCallID, &a.ToolName,
		&a.ToolInput, &a.Status, &a.CreatedAt, &a.DecidedAt)
	return a, err
}

func (s *PGStore) CreateApproval(ctx context.Context, a Approval) (Approval, error) {
	if len(a.ToolInput) == 0 {
		a.ToolInput = json.RawMessage("{}")
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO approvals (run_id, session_id, tool_call_id, tool_name, tool_input)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, run_id, session_id, tool_call_id, tool_name, tool_input, status, created_at, decided_at`,
		a.RunID, a.SessionID, a.ToolCallID, a.ToolName, a.ToolInput)
	ap, err := scanApproval(row)
	if err != nil {
		return Approval{}, fmt.Errorf("create approval: %w", err)
	}
	return ap, nil
}

func (s *PGStore) PendingApprovalForRun(ctx context.Context, runID string) (Approval, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, session_id, tool_call_id, tool_name, tool_input, status, created_at, decided_at
		FROM approvals
		WHERE run_id = $1 AND status = 'pending'
		ORDER BY created_at DESC LIMIT 1`, runID)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("pending approval for run: %w", err)
	}
	return a, true, nil
}

func (s *PGStore) GetApproval(ctx context.Context, id string) (Approval, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, session_id, tool_call_id, tool_name, tool_input, status, created_at, decided_at
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
func (s *PGStore) DecideApproval(ctx context.Context, id string, approve bool) (Approval, error) {
	status := string(ApprovalRejected)
	if approve {
		status = string(ApprovalApproved)
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE approvals SET status = $2, decided_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING id, run_id, session_id, tool_call_id, tool_name, tool_input, status, created_at, decided_at`,
		id, status)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNoPendingApproval
	}
	if err != nil {
		return Approval{}, fmt.Errorf("decide approval: %w", err)
	}
	return a, nil
}
