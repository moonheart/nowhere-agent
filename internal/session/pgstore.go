package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
)

// escapeLike escapes the LIKE/ILIKE wildcards in a user-supplied search term so
// it matches literally (backslash is the default LIKE escape character, so an
// escaped % or _ needs no ESCAPE clause).
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// ErrSessionNotFound reports a hard-delete target that does not exist.
var ErrSessionNotFound = errors.New("session not found")

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

// ListEndedSessionsEndedBefore returns ids of ended sessions whose ended_at
// predates before, ordered by (ended_at, id) ascending, capped at limit — the
// retention sweep's eligibility scan. afterID is the keyset cursor: the last
// id of the previous page ("" for the first page); the page resumes strictly
// after (ended_at, id) of that row. A NULL ended_at (a session ended before
// the column was stamped, or by an old code path) is excluded: unknown age
// means the sweep never touches it. If the cursor session no longer exists,
// the scan returns an empty page (the sweep stops) rather than repeating.
func (s *PGStore) ListEndedSessionsEndedBefore(ctx context.Context, before time.Time, afterID string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text FROM sessions
		WHERE status = $1 AND ended_at IS NOT NULL AND ended_at < $2
		  AND ($4 = '' OR (ended_at, id::text) > (SELECT ended_at, $4::text FROM sessions WHERE id::text = $4))
		ORDER BY ended_at, id
		LIMIT $3`,
		string(SessionEnded), before, limit, afterID)
	if err != nil {
		return nil, fmt.Errorf("list ended sessions: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan ended session: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListUndreamedSessions returns sessions that have messages beyond their
// dreamed watermark — the dreaming worker's eligibility scan (incremental
// model, capability-gap K1). A session qualifies regardless of status (open
// conversations are learnable) while it still has unconsolidated messages.
// Ordered by last activity; the LIMIT bounds a single scan.
func (s *PGStore) ListUndreamedSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.user_id, s.title, s.status, s.created_at, s.updated_at
		FROM sessions s
		WHERE EXISTS (
			SELECT 1 FROM messages m
			WHERE m.session_id = s.id AND m.id > s.dreamed_seq
		)
		ORDER BY s.updated_at
		LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("list undreamed sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan undreamed session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// ListUndreamedSessionsForUser is ListUndreamedSessions narrowed to one owner.
// It backs the user-triggered consolidation on the console: a user asking to
// consolidate "my memories" must not cause anyone else's sessions to be read or
// spent on, so the narrowing happens in SQL rather than by filtering a global
// scan in Go.
func (s *PGStore) ListUndreamedSessionsForUser(ctx context.Context, userID string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.user_id, s.title, s.status, s.created_at, s.updated_at
		FROM sessions s
		WHERE s.user_id = $1 AND EXISTS (
			SELECT 1 FROM messages m
			WHERE m.session_id = s.id AND m.id > s.dreamed_seq
		)
		ORDER BY s.updated_at
		LIMIT 100`, userID)
	if err != nil {
		if identity.IsMalformedID(err) {
			// The id came from a URL or a token claim; a malformed one owns no
			// sessions, which is an empty result rather than a server fault.
			return nil, nil
		}
		return nil, fmt.Errorf("list undreamed sessions for user: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan undreamed session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DreamedSeq returns a session's dreamed watermark (the messages.id the worker
// has consolidated up to). 0 means nothing consolidated yet.
func (s *PGStore) DreamedSeq(ctx context.Context, id string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `
		SELECT dreamed_seq FROM sessions WHERE id = $1`, id).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("get dreamed_seq: %w", err)
	}
	return seq, nil
}

// MarkDreamedSeq advances a session's dreamed watermark, but never backwards
// (GREATEST guards against a stale pass clobbering a newer one). Idempotent.
func (s *PGStore) MarkDreamedSeq(ctx context.Context, id string, seq int64) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET dreamed_seq = GREATEST(dreamed_seq, $2) WHERE id = $1`, id, seq); err != nil {
		return fmt.Errorf("mark dreamed_seq: %w", err)
	}
	return nil
}

// MemoryInjectedAt returns a session's memory-injection watermark. The zero
// time (NULL) means nothing injected yet.
func (s *PGStore) MemoryInjectedAt(ctx context.Context, id string) (time.Time, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT memory_injected_at FROM sessions WHERE id = $1`, id).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("get memory_injected_at: %w", err)
	}
	return at.Time, nil
}

// MarkMemoryInjectedAt advances the watermark, but never backwards (GREATEST
// over COALESCE treats NULL as -infinity so the first mark always lands).
// Idempotent.
func (s *PGStore) MarkMemoryInjectedAt(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET memory_injected_at = GREATEST(COALESCE(memory_injected_at, '-infinity'::timestamptz), $2)
		WHERE id = $1`, id, at); err != nil {
		return fmt.Errorf("mark memory_injected_at: %w", err)
	}
	return nil
}

// SetSessionStateKV upserts one key in the session's state dictionary via
// jsonb_set, so sibling keys are preserved (never a whole-column clobber). This
// is what lets many features share the single state column.
func (s *PGStore) SetSessionStateKV(ctx context.Context, id, key string, value json.RawMessage) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET state = jsonb_set(state, $2, $3, true), updated_at = now()
		WHERE id = $1`,
		id, "{"+key+"}", value)
	if err != nil {
		return fmt.Errorf("set session state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session not found: %s", id)
	}
	return nil
}

// SessionStateKV returns the JSON value stored under key, or false if unset.
func (s *PGStore) SessionStateKV(ctx context.Context, id, key string) (json.RawMessage, bool, error) {
	var v json.RawMessage
	err := s.db.QueryRowContext(ctx, `
		SELECT state -> $2 FROM sessions WHERE id = $1`, id, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("session not found: %s", id)
	}
	if err != nil {
		return nil, false, fmt.Errorf("get session state: %w", err)
	}
	if v == nil || string(v) == "null" {
		return nil, false, nil
	}
	return v, true, nil
}

// SessionState returns the session's whole state dictionary (for history
// recovery, which hands every key to the reloading client).
func (s *PGStore) SessionState(ctx context.Context, id string) (map[string]json.RawMessage, error) {
	var raw json.RawMessage
	err := s.db.QueryRowContext(ctx, `
		SELECT state FROM sessions WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get session state: %w", err)
	}
	out := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode session state: %w", err)
		}
	}
	return out, nil
}

// ListSessionsByUser returns a page of a user's active (non-deleted) sessions,
// most-recently-active first. Ended sessions are hidden from the sidebar. q,
// when non-empty, narrows the list to sessions whose title contains it
// (case-insensitive, ILIKE; wildcards in the term are matched literally).
// Pagination is keyset: the query fetches limit+1 rows, the extra one proving
// that another page exists, and NextCursor pins (updated_at, id) of the page's
// last row (id breaks ties between sessions updated in the same instant).
func (s *PGStore) ListSessionsByUser(ctx context.Context, userID string, q string, limit int, cursor *SessionCursor) (SessionPage, error) {
	if limit <= 0 {
		limit = 25
	}
	query := `
		SELECT id, user_id, title, status, created_at, updated_at
		FROM sessions
		WHERE user_id = $1 AND status = $2`
	args := []any{userID, string(SessionActive)}
	if q != "" {
		// The leading wildcard makes the pattern non-prefix, so a plain btree
		// index on title cannot serve it. Acceptable here: the scan is confined
		// to ONE user's active sessions, a small set per user, so a sequential
		// scan is cheap at the scale this serves. If it ever grows, a trigram
		// GIN index (pg_trgm) is the upgrade path — no query change needed.
		query += ` AND title ILIKE '%' || $` + strconv.Itoa(len(args)+1) + ` || '%'`
		args = append(args, escapeLike(q))
	}
	if cursor != nil {
		query += ` AND (updated_at, id) < ($` + strconv.Itoa(len(args)+1) + `::timestamptz, $` + strconv.Itoa(len(args)+2) + `::uuid)`
		args = append(args, cursor.UpdatedAt, cursor.ID)
	}
	query += `
		ORDER BY updated_at DESC, id DESC
		LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return SessionPage{}, fmt.Errorf("list sessions by user: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return SessionPage{}, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, fmt.Errorf("list sessions by user: %w", err)
	}

	page := SessionPage{}
	if len(out) > limit {
		last := out[limit-1]
		page.Sessions = out[:limit]
		page.NextCursor = &SessionCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	} else {
		page.Sessions = out
	}
	return page, nil
}

// DeleteSessionForUser soft-deletes (ends) a session owned by userID.
func (s *PGStore) DeleteSessionForUser(ctx context.Context, id, userID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET status = $3, ended_at = now(), updated_at = now()
		WHERE id = $1 AND user_id = $2`, id, userID, string(SessionEnded))
	if err != nil {
		return false, fmt.Errorf("delete session: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteSession hard-deletes one session row (runs, messages, events,
// approvals, suspended batches cascade via FK). A missing row maps to
// ErrSessionNotFound. Parameterized by id only — never a bulk delete. A
// malformed (non-uuid) id reads as not-found, matching the identity store's
// convention so probes and typos never surface as server faults.
func (s *PGStore) DeleteSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if identity.IsMalformedID(err) {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("hard delete session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// SessionIDsForUser returns every session id a user owns (any status), so the
// admin purge can remove their workspace image dirs before the user row (and
// with it the session rows) is gone.
func (s *PGStore) SessionIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text FROM sessions WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		if identity.IsMalformedID(err) {
			return nil, nil // a malformed id owns nothing
		}
		return nil, fmt.Errorf("session ids for user: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session id: %w", err)
		}
		out = append(out, id)
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

// SetRunAttribution stamps the run's billing attribution (enterprise-readiness
// P1-3): the team whose provider key paid for the run, and the model it ran.
// An empty teamID stores NULL (platform-billed), so team-grouped reports stay
// exact rather than absorbing platform spend. Best-effort at submit: a failure
// here must not block the run, so callers log and continue.
func (s *PGStore) SetRunAttribution(ctx context.Context, runID, teamID, model string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET team_id = NULLIF($2, '')::uuid, model = NULLIF($3, '')
		WHERE id = $1`, runID, teamID, model)
	if err != nil {
		return fmt.Errorf("set run attribution: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run not found: %s", runID)
	}
	return nil
}

// SetRunUsage records the run's aggregate token usage. u is nil-safe (a no-op).
func (s *PGStore) SetRunUsage(ctx context.Context, runID string, u *provider.Usage) error {
	if u == nil {
		return nil
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET usage_input = $2, usage_output = $3,
			usage_cache_read = $4, usage_cache_write = $5
		WHERE id = $1`,
		runID, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	if err != nil {
		return fmt.Errorf("set run usage: %w", err)
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
		WHERE session_id = $1 AND status IN ('queued','running')
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

// FailStrandedRuns marks all non-terminal runs failed (startup reconciliation
// for runs whose owning process died mid-run). Runs are stateless and terminal
// on completion (capability-gap O2 run-stateless model): any queued/running row
// at startup belongs to a dead worker, so it is failed. This also clears any
// pre-refactor waiting_approval rows. Returns the number of runs updated.
func (s *PGStore) FailStrandedRuns(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = 'failed', finished_at = now()
		WHERE status IN ('queued','running','waiting_approval')`)
	if err != nil {
		return 0, fmt.Errorf("fail stranded runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// StrandedRuns returns every non-terminal run (queued/running/
// waiting_approval), newest first — startup recovery inspects each run's step
// intents before settling it (change durable-run-accounting).
func (s *PGStore) StrandedRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, seq, status, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write
		FROM runs
		WHERE status IN ('queued','running','waiting_approval')
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("stranded runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendRunStep writes one step intent BEFORE the effect it accounts for. seq
// is the run's next step ordinal; attempt is the durable per-(run, step_kind)
// counter (1-based), so a crash-restart loop can never reset it. For step kinds
// that produce a message (assistant, tool), a fresh messages.id is provisioned
// from the messages sequence and returned on the step; the effect's result
// message must be inserted with exactly that id. overflow_compact rows carry no
// provisioned id. Callers MUST serialize appends for one run (the run worker
// owns the run, so a registry-level mutex suffices).
func (s *PGStore) AppendRunStep(ctx context.Context, runID string, kind StepKind, toolCallID string, resultMessageID *int64) (RunStep, error) {
	var st RunStep
	var provision *int64
	var scannedToolCallID sql.NullString
	if kind == StepAssistant || kind == StepTool {
		if resultMessageID != nil {
			// A parallel tool batch reuses the first call's provisioned id: the
			// batch's results land in one tool-result message.
			provision = resultMessageID
		} else {
			if err := s.db.QueryRowContext(ctx,
				`SELECT nextval('messages_id_seq')`).Scan(&provision); err != nil {
				return RunStep{}, fmt.Errorf("provision message id: %w", err)
			}
		}
	}
	tcID := sql.NullString{String: toolCallID, Valid: toolCallID != ""}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO run_steps (run_id, seq, step_kind, attempt, result_message_id, tool_call_id)
		VALUES ($1,
			(SELECT COALESCE(MAX(seq), 0) + 1 FROM run_steps WHERE run_id = $1),
			$2,
			(SELECT COALESCE(MAX(attempt), 0) + 1 FROM run_steps WHERE run_id = $1 AND step_kind = $2),
			$3, NULLIF($4, ''))
		RETURNING id, run_id, seq, step_kind, attempt, result_message_id, tool_call_id, created_at`,
		runID, string(kind), provision, tcID,
	).Scan(&st.ID, &st.RunID, &st.Seq, (*stepKindString)(&st.StepKind), &st.Attempt, &st.ResultMessageID, &scannedToolCallID, &st.CreatedAt)
	if scannedToolCallID.Valid {
		st.ToolCallID = scannedToolCallID.String
	}
	if err != nil {
		return RunStep{}, fmt.Errorf("append run step: %w", err)
	}
	return st, nil
}

// stepKindString adapts a *StepKind for database/sql scanning.
type stepKindString StepKind

func (k *stepKindString) Scan(value any) error {
	s, ok := value.(string)
	if !ok {
		return fmt.Errorf("scan step_kind: unexpected type %T", value)
	}
	*k = stepKindString(StepKind(s))
	return nil
}

// AppendUsageRecord writes one per-request usage row at settle time, before any
// classification, retry, or discard decision. resultMessageID binds the record
// to the pre-provisioned id of the message the request was expected to produce;
// the message may never exist (failed/discarded requests), the binding holds.
func (s *PGStore) AppendUsageRecord(ctx context.Context, rec UsageRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_records (run_id, cause, result_message_id, attempt,
			input, output, cache_read, cache_write)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		rec.RunID, string(rec.Cause), rec.ResultMessageID, rec.Attempt,
		rec.Usage.InputTokens, rec.Usage.OutputTokens, rec.Usage.CacheReadTokens, rec.Usage.CacheWriteTokens)
	if err != nil {
		return fmt.Errorf("append usage record: %w", err)
	}
	return nil
}

// SumUsage returns a run's aggregate usage as the sum of its ledger rows. A run
// with no rows returns a zero usage (never nil), so callers can aggregate
// unconditionally.
func (s *PGStore) SumUsage(ctx context.Context, runID string) (*provider.Usage, error) {
	var in, out, cr, cw sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input), 0), COALESCE(SUM(output), 0),
			COALESCE(SUM(cache_read), 0), COALESCE(SUM(cache_write), 0)
		FROM usage_records WHERE run_id = $1`, runID).
		Scan(&in, &out, &cr, &cw)
	if err != nil {
		return nil, fmt.Errorf("sum usage: %w", err)
	}
	return &provider.Usage{
		InputTokens:      int(in.Int64),
		OutputTokens:     int(out.Int64),
		CacheReadTokens:  int(cr.Int64),
		CacheWriteTokens: int(cw.Int64),
	}, nil
}

// LatestRunSteps returns a run's newest intent rows (newest first), with
// ResultExists populated by a left join against messages — recovery reads the
// step's provisioned id and learns at once whether the result message landed.
// overflow_compact rows are excluded: they are completed recovery records, not
// pending effects, so they can never be the "newest unfinished step".
func (s *PGStore) LatestRunSteps(ctx context.Context, runID string, limit int) ([]RunStep, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT rs.id, rs.run_id, rs.seq, rs.step_kind, rs.attempt, rs.result_message_id,
			rs.tool_call_id, rs.created_at,
			(rs.result_message_id IS NOT NULL AND m.id IS NOT NULL) AS result_exists
		FROM run_steps rs
		LEFT JOIN messages m ON m.id = rs.result_message_id
		WHERE rs.run_id = $1 AND rs.step_kind IN ('assistant','tool')
		ORDER BY rs.seq DESC
		LIMIT $2`, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("latest run steps: %w", err)
	}
	defer rows.Close()

	var out []RunStep
	for rows.Next() {
		var st RunStep
		var scannedToolCallID sql.NullString
		if err := rows.Scan(&st.ID, &st.RunID, &st.Seq, (*stepKindString)(&st.StepKind), &st.Attempt,
			&st.ResultMessageID, &scannedToolCallID, &st.CreatedAt, &st.ResultExists); err != nil {
			return nil, fmt.Errorf("scan run step: %w", err)
		}
		if scannedToolCallID.Valid {
			st.ToolCallID = scannedToolCallID.String
		}
		out = append(out, st)
	}
	return out, rows.Err()
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
		SELECT id, session_id, seq, status, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write
		FROM runs
		WHERE session_id = $1
		ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("runs for session: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanRun reads one run row including its usage cols, rebuilding Run.Usage when
// usage was recorded (usage_input non-NULL).
func scanRun(rows *sql.Rows) (Run, error) {
	var r Run
	var in, out, cr, cw sql.NullInt64
	if err := rows.Scan(&r.ID, &r.SessionID, &r.Seq, &r.Status, &r.CreatedAt, &in, &out, &cr, &cw); err != nil {
		return Run{}, fmt.Errorf("scan run: %w", err)
	}
	if in.Valid {
		r.Usage = &provider.Usage{
			InputTokens:      int(in.Int64),
			OutputTokens:     int(out.Int64),
			CacheReadTokens:  int(cr.Int64),
			CacheWriteTokens: int(cw.Int64),
		}
	}
	return r, nil
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
