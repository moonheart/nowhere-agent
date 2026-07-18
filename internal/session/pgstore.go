package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store persists sessions, runs, and events in Postgres. The durable run
// records double as the episodes the dreaming worker consumes (design D13).
type PGStore struct {
	db *sql.DB
}

// NewPGStore creates a Postgres-backed Store.
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

// CreateSession inserts an active session for a user.
func (s *PGStore) CreateSession(ctx context.Context, userID, title string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO sessions (user_id, title)
		VALUES ($1, $2)
		RETURNING id, user_id, title, status, created_at, updated_at`,
		userID, title,
	).Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// GetSession fetches a session by id.
func (s *PGStore) GetSession(ctx context.Context, id string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, title, status, created_at, updated_at
		FROM sessions WHERE id = $1`, id).
		Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("session not found: %s", id)
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

// EndSession marks a session ended.
func (s *PGStore) EndSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET status = $2, ended_at = now(), updated_at = now()
		WHERE id = $1`, id, string(SessionEnded))
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}

// ListIdleSessions returns active sessions whose last activity (updated_at) is
// before the given time — candidates for idle-end by the scheduler.
func (s *PGStore) ListIdleSessions(ctx context.Context, idleSinceEventBefore time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, title, status, created_at, updated_at
		FROM sessions
		WHERE status = $1 AND updated_at < $2
		ORDER BY updated_at`,
		string(SessionActive), idleSinceEventBefore)
	if err != nil {
		return nil, fmt.Errorf("list idle sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan idle session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// CreateRun inserts a queued run with the given per-session sequence number.
func (s *PGStore) CreateRun(ctx context.Context, sessionID string, seq int) (Run, error) {
	var r Run
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO runs (session_id, seq)
		VALUES ($1, $2)
		RETURNING id, session_id, seq, status, created_at`,
		sessionID, seq,
	).Scan(&r.ID, &r.SessionID, &r.Seq, &r.Status, &r.CreatedAt)
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", err)
	}
	return r, nil
}

// UpdateRunStatus sets a run's status, stamping finished_at on terminal states.
func (s *PGStore) UpdateRunStatus(ctx context.Context, runID string, status RunStatus) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = $2,
			finished_at = CASE WHEN $2 IN ('done','failed','cancelled') THEN now() ELSE finished_at END
		WHERE id = $1`, runID, string(status))
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run not found: %s", runID)
	}
	return nil
}

// ActiveRun returns the in-progress run for a session, or false.
func (s *PGStore) ActiveRun(ctx context.Context, sessionID string) (Run, bool, error) {
	var r Run
	err := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, seq, status, created_at
		FROM runs
		WHERE session_id = $1 AND status IN ('queued','running','waiting_approval')
		ORDER BY seq DESC LIMIT 1`, sessionID).
		Scan(&r.ID, &r.SessionID, &r.Seq, &r.Status, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("active run: %w", err)
	}
	return r, true, nil
}

// NextRunSeq returns the next sequence number for a session's run.
func (s *PGStore) NextRunSeq(ctx context.Context, sessionID string) (int, error) {
	var next int
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1 FROM runs WHERE session_id = $1`, sessionID).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("next run seq: %w", err)
	}
	return next, nil
}

// RunsForSession returns all runs in a session ordered by seq, for history replay.
func (s *PGStore) RunsForSession(ctx context.Context, sessionID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, seq, status, created_at
		FROM runs
		WHERE session_id = $1
		ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("runs for session: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Seq, &r.Status, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendEvent persists one run event (flushing an iteration to the DB).
func (s *PGStore) AppendEvent(ctx context.Context, e Event) error {
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO run_events (run_id, session_id, seq_offset, kind, payload)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`,
		e.RunID, e.SessionID, e.Offset, e.Kind, e.Payload,
	).Scan(&e.CreatedAt)
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}

	// Touch the session so idle detection sees the activity.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET updated_at = now() WHERE id = $1`, e.SessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// EventsAfter returns events for a run with offset > after, ordered by offset.
func (s *PGStore) EventsAfter(ctx context.Context, runID string, after int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, session_id, seq_offset, kind, payload, created_at
		FROM run_events
		WHERE run_id = $1 AND seq_offset > $2
		ORDER BY seq_offset`, runID, after)
	if err != nil {
		return nil, fmt.Errorf("events after: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.RunID, &e.SessionID, &e.Offset, &e.Kind, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
