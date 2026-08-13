package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	// seq is a store-assigned insertion counter giving a batch a stable queue
	// order when CreatedAt ties (a batch created in one tick). 0 for PG rows,
	// which order by created_at,ctid instead. Unexported: not a DB column.
	seq int64
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

// sortInteractions orders interactions by creation order (queue order: earliest
// first). The in-memory seq counter is authoritative when set; otherwise fall
// back to CreatedAt then id so a same-timestamp batch is deterministic.
func sortInteractions(is []Interaction) {
	sort.SliceStable(is, func(i, j int) bool {
		if is[i].seq != 0 && is[j].seq != 0 {
			return is[i].seq < is[j].seq
		}
		if is[i].CreatedAt.Equal(is[j].CreatedAt) {
			return is[i].ID < is[j].ID
		}
		return is[i].CreatedAt.Before(is[j].CreatedAt)
	})
}

// --- MemStore (in-memory, tests/dev) ---

func (m *MemStore) CreateApproval(_ context.Context, a Interaction) (Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Multiple pending interactions per session are allowed (multi-approval
	// queue): a gated batch parks one interaction per gated call.
	return m.createApprovalLocked(a), nil
}

func (m *MemStore) PendingApprovalForSession(_ context.Context, sessionID string) (Interaction, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var head *Interaction
	for _, a := range m.approvals {
		if a.SessionID == sessionID && a.Status == InteractionPending {
			// CreatedAt ties (a batch created in one tick) must still pick the
			// queue head deterministically: seq is the insertion counter, the
			// same tie-break sortInteractions uses.
			if head == nil || a.CreatedAt.Before(head.CreatedAt) ||
				(a.CreatedAt.Equal(head.CreatedAt) && a.seq < head.seq) {
				head = a
			}
		}
	}
	if head == nil {
		return Interaction{}, false, nil
	}
	return *head, true, nil
}

// PendingApprovalsForSession returns every pending interaction for a session in
// queue order (earliest created first) — the full gated batch a reloading client
// must re-render, not just the head.
func (m *MemStore) PendingApprovalsForSession(_ context.Context, sessionID string) ([]Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Interaction
	for _, a := range m.approvals {
		if a.SessionID == sessionID && a.Status == InteractionPending {
			out = append(out, *a)
		}
	}
	sortInteractions(out)
	return out, nil
}

// PendingApprovalsForRun returns the still-pending interactions of one run (one
// gated batch), in queue order. Empty means the batch is fully resolved — the
// signal that a fresh run may resume the conversation.
func (m *MemStore) PendingApprovalsForRun(_ context.Context, runID string) ([]Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Interaction
	for _, a := range m.approvals {
		if a.RunID == runID && a.Status == InteractionPending {
			out = append(out, *a)
		}
	}
	sortInteractions(out)
	return out, nil
}

// ApprovalsForRun returns ALL interactions of one run (one gated batch), any
// status, in queue order. Used to fold a fully-resolved batch into tool_results.
func (m *MemStore) ApprovalsForRun(_ context.Context, runID string) ([]Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Interaction
	for _, a := range m.approvals {
		if a.RunID == runID {
			out = append(out, *a)
		}
	}
	sortInteractions(out)
	return out, nil
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

// SweepExpiredInteractions marks every interaction still pending since before
// cutoff as expired. It is the store half of the pending-gate reaper (the
// hourly loop lives in cmd/server): without it a client that never decides
// locks the session's pending-interaction gate forever (every new submission
// answers 409 pending_interaction). Expired rows keep their decided_at NULL —
// they were never decided, only aged out; a fold of an expired batch reports
// each call as rejected.
func (m *MemStore) SweepExpiredInteractions(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, a := range m.approvals {
		if a.Status == InteractionPending && a.CreatedAt.Before(cutoff) {
			a.Status = InteractionExpired
			n++
		}
	}
	return n, nil
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
		ORDER BY created_at ASC, ctid ASC LIMIT 1`, sessionID)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Interaction{}, false, nil
	}
	if err != nil {
		return Interaction{}, false, fmt.Errorf("pending interaction for session: %w", err)
	}
	return a, true, nil
}

// PendingApprovalsForSession returns every pending interaction for a session in
// queue order (earliest created first) — the full gated batch a reloading client
// must re-render, not just the head.
func (s *PGStore) PendingApprovalsForSession(ctx context.Context, sessionID string) ([]Interaction, error) {
	return s.queryInteractions(ctx, `
		SELECT `+approvalCols+`
		FROM approvals
		WHERE session_id = $1 AND status = 'pending'
		ORDER BY created_at ASC, ctid ASC`, sessionID)
}

// PendingApprovalsForRun returns the still-pending interactions of one run (one
// gated batch), in queue order. Empty means the batch is fully resolved — the
// signal that a fresh run may resume the conversation.
func (s *PGStore) PendingApprovalsForRun(ctx context.Context, runID string) ([]Interaction, error) {
	return s.queryInteractions(ctx, `
		SELECT `+approvalCols+`
		FROM approvals
		WHERE run_id = $1 AND status = 'pending'
		ORDER BY created_at ASC, ctid ASC`, runID)
}

// ApprovalsForRun returns ALL interactions of one run (one gated batch), any
// status, in queue order. Used to fold a fully-resolved batch into tool_results.
func (s *PGStore) ApprovalsForRun(ctx context.Context, runID string) ([]Interaction, error) {
	return s.queryInteractions(ctx, `
		SELECT `+approvalCols+`
		FROM approvals
		WHERE run_id = $1
		ORDER BY created_at ASC, ctid ASC`, runID)
}

// queryInteractions runs a pending-interactions SELECT and scans the rows.
func (s *PGStore) queryInteractions(ctx context.Context, query string, arg any) ([]Interaction, error) {
	rows, err := s.db.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("query interactions: %w", err)
	}
	defer rows.Close()
	var out []Interaction
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("scan interaction: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
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

// SweepExpiredInteractions marks every interaction still pending since before
// cutoff as expired (the pending-gate reaper's store half; the hourly loop
// lives in cmd/server). A client that never decides must not lock the
// session's pending-interaction gate forever — every new submission would
// answer 409 pending_interaction. Expired rows keep decided_at NULL (never
// decided, only aged out); if the client later returns, DecideApproval refuses
// the stale verdict (status is no longer pending) and a fold reports each
// expired call as rejected.
func (s *PGStore) SweepExpiredInteractions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = 'expired'
		WHERE status = 'pending' AND created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sweep expired interactions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// nullableJSON maps an empty result to SQL NULL (the answer column is nullable).
func nullableJSON(j json.RawMessage) any {
	if len(j) == 0 {
		return nil
	}
	return j
}
