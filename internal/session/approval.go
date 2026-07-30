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

// InteractionStatus is the lifecycle of a pending interaction.
type InteractionStatus string

const (
	InteractionPending  InteractionStatus = "pending"
	InteractionResolved InteractionStatus = "resolved"
	InteractionRejected InteractionStatus = "rejected"
	InteractionExpired  InteractionStatus = "expired"
)

// InteractionKind names the built-in interaction kinds. Kind is an OPEN string
// — a registered InteractionHandler interprets the payload/result for each —
// so new kinds are added by registering a handler, not by editing this list.
const (
	// KindToolApproval is a dangerous call gated for a yes/no verdict.
	KindToolApproval = "approval"
	// KindAskUser is the model asking structured questions.
	KindAskUser = "ask_user"
	// KindClientTool is a tool the client executes (not the server).
	KindClientTool = "client_tool"
)

// Interaction is the durable record of a run suspended waiting on the client —
// the general interrupt (capability-gap O2 + O-ask + client-side tools,
// migration 000010/000015). It generalizes the former Approval: a run surfaces
// a tool call that needs client interaction (a permission approval, an ask_user
// question set, or a client-side tool), parks the payload here, and ends; the
// decision endpoint resolves the row and a fresh run folds the result back. The
// pause survives process restarts and works across instances.
type Interaction struct {
	ID         string
	RunID      string
	SessionID  string
	ToolCallID string
	ToolName   string
	// Payload is what the client is shown: for an approval, the gated call's args
	// (re-executed on approve); for ask_user, the question set; for a client
	// tool, the call's input. (DB column: tool_input.)
	Payload json.RawMessage
	// Kind is the open interaction kind (see the Kind* constants). Empty = approval.
	Kind   string
	Status InteractionStatus
	// Result is what the client returns: an ask_user answer ({"answers":{...}})
	// or a client-tool output/error. NULL for a permission approval (the verdict
	// is in Status). (DB column: answer.)
	Result    json.RawMessage
	CreatedAt time.Time
	DecidedAt *time.Time
}

// Approval is retained as an alias of Interaction for source compatibility
// during the transition to the general-interrupt model.
type Approval = Interaction

// ApprovalStatus aliases keep the pre-generalization names compiling.
type ApprovalStatus = InteractionStatus

const (
	ApprovalPending  = InteractionPending
	ApprovalApproved = InteractionResolved
	ApprovalRejected = InteractionRejected
	ApprovalExpired  = InteractionExpired
)

// ErrNoPendingApproval is returned when resolving/fetching an interaction that
// is not pending (unknown id, or already decided).
var ErrNoPendingApproval = errors.New("no pending approval")

// ErrNoPendingInteraction is the general name for ErrNoPendingApproval.
var ErrNoPendingInteraction = ErrNoPendingApproval

// --- MemStore (in-memory, tests/dev) ---

func (m *MemStore) CreateApproval(_ context.Context, a Interaction) (Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.approvals {
		if ex.SessionID == a.SessionID && ex.Status == InteractionPending {
			return Interaction{}, fmt.Errorf("session %s already has a pending interaction", a.SessionID)
		}
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	a.Status = InteractionPending
	a.CreatedAt = time.Now()
	cp := a
	m.approvals[a.ID] = &cp
	return a, nil
}

func (m *MemStore) PendingApprovalForSession(_ context.Context, sessionID string) (Interaction, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.approvals {
		if a.SessionID == sessionID && a.Status == InteractionPending {
			return *a, true, nil
		}
	}
	return Interaction{}, false, nil
}

func (m *MemStore) GetApproval(_ context.Context, id string) (Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Interaction{}, ErrNoPendingApproval
	}
	return *a, nil
}

func (m *MemStore) DecideApproval(_ context.Context, id string, approve bool, result json.RawMessage) (Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok || a.Status != InteractionPending {
		return Interaction{}, ErrNoPendingApproval
	}
	now := time.Now()
	a.DecidedAt = &now
	a.Result = result
	if approve {
		a.Status = InteractionResolved
	} else {
		a.Status = InteractionRejected
	}
	return *a, nil
}

// --- PGStore (Postgres, production) ---

// approvalCols is the shared column list for approvals scans (kind + answer
// included so every read returns the full record). tool_input maps to Payload,
// answer to Result.
const approvalCols = `id, run_id, session_id, tool_call_id, tool_name, tool_input, kind, status, answer, created_at, decided_at`

// scanApproval reads one approvals row. answer (Result) is nullable in the DB,
// so it scans through a []byte (nil-safe) rather than json.RawMessage, which the
// driver cannot store a NULL into.
func scanApproval(row interface{ Scan(...any) error }) (Interaction, error) {
	var a Interaction
	var result []byte
	err := row.Scan(&a.ID, &a.RunID, &a.SessionID, &a.ToolCallID, &a.ToolName,
		&a.Payload, &a.Kind, &a.Status, &result, &a.CreatedAt, &a.DecidedAt)
	a.Result = result
	return a, err
}

func (s *PGStore) CreateApproval(ctx context.Context, a Interaction) (Interaction, error) {
	if len(a.Payload) == 0 {
		a.Payload = json.RawMessage("{}")
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	kind := a.Kind
	if kind == "" {
		kind = KindToolApproval
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO approvals (id, run_id, session_id, tool_call_id, tool_name, tool_input, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+approvalCols,
		a.ID, a.RunID, a.SessionID, a.ToolCallID, a.ToolName, a.Payload, kind)
	ap, err := scanApproval(row)
	if err != nil {
		return Interaction{}, fmt.Errorf("create interaction: %w", err)
	}
	return ap, nil
}

func (s *PGStore) PendingApprovalForSession(ctx context.Context, sessionID string) (Interaction, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+approvalCols+`
		FROM approvals
		WHERE session_id = $1 AND status = 'pending'
		ORDER BY created_at DESC LIMIT 1`, sessionID)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Interaction{}, false, nil
	}
	if err != nil {
		return Interaction{}, false, fmt.Errorf("pending interaction for session: %w", err)
	}
	return a, true, nil
}

func (s *PGStore) GetApproval(ctx context.Context, id string) (Interaction, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+approvalCols+`
		FROM approvals WHERE id = $1`, id)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Interaction{}, ErrNoPendingApproval
	}
	if err != nil {
		return Interaction{}, fmt.Errorf("get interaction: %w", err)
	}
	return a, nil
}

// DecideApproval atomically resolves a pending interaction (optimistic: the
// status='pending' predicate makes a concurrent double-decide a no-op → error).
// approve=true → resolved, false → rejected. result is the client-returned value
// (ask_user answer / client-tool output); nil for a permission approval.
func (s *PGStore) DecideApproval(ctx context.Context, id string, approve bool, result json.RawMessage) (Interaction, error) {
	status := string(InteractionRejected)
	if approve {
		status = string(InteractionResolved)
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE approvals SET status = $2, answer = $3, decided_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+approvalCols,
		id, status, nullableJSON(result))
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Interaction{}, ErrNoPendingApproval
	}
	if err != nil {
		return Interaction{}, fmt.Errorf("decide interaction: %w", err)
	}
	return a, nil
}

// nullableJSON maps an empty result to SQL NULL (the answer column is nullable).
func nullableJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return j
}
