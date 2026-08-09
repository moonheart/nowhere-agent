// Package session implements the session-runtime capability (design D13): run
// lifecycle state machine decoupled from transport, durable run event log
// (which doubles as the episodes for dreaming), single-active-run enforcement
// with multi-client state sync, and session lifecycle (idle-end detection).
package session

import (
	"encoding/json"
	"time"

	"nowhere-agent/internal/provider"
)

// RunStatus is the run lifecycle state.
type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunDone      RunStatus = "done"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// Valid reports whether s is a known run status.
func (s RunStatus) Valid() bool {
	switch s {
	case RunQueued, RunRunning, RunDone, RunFailed, RunCancelled:
		return true
	}
	return false
}

// Terminal reports whether the run can no longer change state.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunDone, RunFailed, RunCancelled:
		return true
	}
	return false
}

// Active reports whether the run is in progress (blocking new runs).
func (s RunStatus) Active() bool {
	switch s {
	case RunQueued, RunRunning:
		return true
	}
	return false
}

// SessionStatus is the session lifecycle state.
type SessionStatus string

const (
	SessionActive SessionStatus = "active"
	SessionEnded  SessionStatus = "ended"
)

// Session is a conversation between a user and the agent.
type Session struct {
	ID        string
	UserID    string
	Title     string
	Status    SessionStatus
	CreatedAt time.Time
	UpdatedAt time.Time
	// State is the session's generic key/value store (capability-gap O1): an
	// open dictionary of session-scoped state, one JSON value per key. plan/todo
	// is the first consumer ("plan"); any feature can add its own key without a
	// schema change. Persisted as the sessions.state JSONB column. Nil means no
	// state has been written yet.
	State map[string]json.RawMessage
}

// SessionCursor is the keyset position of the last session in a page of the
// conversation list, used to fetch the next page. It pins (updated_at, id):
// the list is ordered by updated_at DESC with id as a tiebreaker, so every
// page continues strictly below this pair.
type SessionCursor struct {
	UpdatedAt time.Time
	ID        string
}

// SessionPage is one page of a user's conversation list: the sessions in the
// page plus the cursor to fetch the next one (nil when the list is exhausted).
type SessionPage struct {
	Sessions   []Session
	NextCursor *SessionCursor
}

// Run is one agent turn-chain within a session.
type Run struct {
	ID        string
	SessionID string
	Seq       int
	Status    RunStatus
	CreatedAt time.Time
	// TeamID attributes the run to the team whose provider key billed it
	// (enterprise-readiness P1-3). Empty when the run fell back to the platform
	// key. It records attribution, not membership, so it survives the team being
	// deleted and is NOT a foreign key. Nil-vs-empty is not distinguished in
	// memory; the column is nullable in Postgres only so old rows read NULL.
	TeamID string
	// Model is the model the run's loop was configured with, stamped at submit
	// so per-model breakdown and cost estimation need not guess.
	Model string
	// Usage aggregates the run's token consumption across all its LLM calls
	// (redundant with SUM of its messages' usage, kept for cheap queries). Nil
	// until the run reports usage. Persisted as the runs.usage_* cols.
	Usage *provider.Usage
}

// Event is one persisted run event (an episode record / replay unit).
type Event struct {
	RunID     string
	SessionID string
	Offset    int
	Kind      string
	Payload   []byte // JSON
	CreatedAt time.Time
}
