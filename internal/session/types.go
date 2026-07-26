// Package session implements the session-runtime capability (design D13): run
// lifecycle state machine decoupled from transport, durable run event log
// (which doubles as the episodes for dreaming), single-active-run enforcement
// with multi-client state sync, and session lifecycle (idle-end detection).
package session

import (
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
}

// Run is one agent turn-chain within a session.
type Run struct {
	ID        string
	SessionID string
	Seq       int
	Status    RunStatus
	CreatedAt time.Time
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
