package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
)

// ErrNotFound is returned for a task (or session) id that does not exist,
// including malformed ids (see identity.IsMalformedID).
var ErrNotFound = errors.New("schedule: not found")

// ProducedSession is one session a task created, with the display fields the
// console renders (the internal ListSessions returns bare ids).
type ProducedSession struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists scheduled tasks. PG and in-memory implementations share the
// contract so the trigger and HTTP layer are backend-agnostic (ports &
// adapters). Times are stored and compared in absolute instants; the cron
// timezone only affects how NextRunAt is computed, never how it is stored.
type Store interface {
	// Create validates and inserts a task, seeding NextRunAt from its cron and
	// timezone (the first fire is the next occurrence after now).
	Create(ctx context.Context, t Task) (Task, error)
	// Get fetches one task by id, ErrNotFound when absent.
	Get(ctx context.Context, id string) (Task, error)
	// Update applies mutable fields (schedule, prompt, whitelist, flags) and
	// recomputes NextRunAt from the new schedule relative to now. It returns
	// ErrNotFound for an absent id.
	Update(ctx context.Context, t Task) (Task, error)
	// Delete removes a task, ErrNotFound when absent. Sessions it produced keep
	// their rows (task_id is ON DELETE SET NULL).
	Delete(ctx context.Context, id string) error
	// SetEnabled toggles a task without touching its schedule, ErrNotFound when
	// absent. Disabling is the reversible alternative to Delete.
	SetEnabled(ctx context.Context, id string, enabled bool) error
	// ListForUser returns tasks owned by the user plus tasks in teams the user
	// belongs to (the caller-visible scope), newest first.
	ListForUser(ctx context.Context, userID string) ([]Task, error)
	// ListDue returns enabled tasks whose NextRunAt is at or before now and
	// which are not past EndTime — the trigger's scan set.
	ListDue(ctx context.Context, now time.Time) ([]Task, error)
	// Claim atomically advances a still-due task's NextRunAt to the next future
	// occurrence and records LastRunAt, returning the claimed task. It returns
	// ErrNotFound when the row no longer qualifies (disabled, expired, or
	// already claimed by a racing instance) — the caller treats that as "skip",
	// not an error (design D4).
	Claim(ctx context.Context, id string, now time.Time) (Task, error)
	// RequeueDue pushes a claimed-but-skipped task's NextRunAt back to `now` so
	// the next scan retries it. Claim already advanced the schedule; a
	// pre-submit skip (budget gate, pending interaction) must not burn the slot
	// the claim consumed — a daily task would otherwise wait 24h for its next
	// chance. It only applies when the task still matches what the claim
	// returned: t.NextRunAt must still be the stored next_run_at and the task
	// must still be enabled and unexpired at `now` — an operator edit (cron,
	// disable, end time) between claim and requeue makes the requeue a no-op
	// (ErrNotFound) instead of clobbering the fresh schedule or resurrecting a
	// disabled/expired task. ErrNotFound is also returned when the task no
	// longer exists (deleted between claim and requeue — a fine no-op).
	RequeueDue(ctx context.Context, t Task, now time.Time) error
	// ListSessions returns the ids of active sessions a task produced, newest
	// first. Ended (cleared) sessions are hidden, matching the sidebar's rule.
	ListSessions(ctx context.Context, taskID string) ([]string, error)
	// EndSessions soft-deletes every active session a task produced (status →
	// ended), returning how many were cleared. Soft-delete matches the chat
	// delete path: rows stay for audit and dreaming; only the sidebar/this list
	// hide them. Clearing is confined to active sessions so it is idempotent.
	EndSessions(ctx context.Context, taskID string) (int, error)
}

// ---------------------------------------------------------------------------
// Postgres implementation
// ---------------------------------------------------------------------------

// PGStore is a Postgres-backed Store.
type PGStore struct {
	db *sql.DB
}

// NewPGStore creates a Postgres-backed Store.
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

// taskCols is the canonical column list shared by every read, in scan order.
const taskCols = `id, user_id, COALESCE(team_id::text,''), COALESCE(agent_def_name,''), COALESCE(prompt,''),
	tool_whitelist, cron, timezone, COALESCE(target_session_id::text,''), on_run_completed, multitask_strategy,
	COALESCE(webhook_url,''), end_time, enabled, next_run_at, last_run_at, metadata, created_at, updated_at`

// Create inserts a validated task with its first NextRunAt seeded.
func (s *PGStore) Create(ctx context.Context, t Task) (Task, error) {
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	next, err := t.NextAfter(time.Now())
	if err != nil {
		return Task{}, err
	}
	t.NextRunAt = next

	meta, err := json.Marshal(nonNilMeta(t.Metadata))
	if err != nil {
		return Task{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_task
			(user_id, team_id, agent_def_name, prompt, tool_whitelist, cron, timezone,
			 target_session_id, on_run_completed, multitask_strategy, webhook_url, end_time, enabled,
			 next_run_at, metadata)
		VALUES ($1, NULLIF($2,'')::uuid, NULLIF($3,''), NULLIF($4,''), $5::text[], $6, $7,
			 NULLIF($8,'')::uuid, $9, $10, NULLIF($11,''), $12, $13, $14, $15)
		RETURNING `+taskCols,
		t.UserID, t.TeamID, t.AgentDefName, t.Prompt, formatTextArray(t.ToolWhitelist), t.Cron, tzOr(t.Timezone),
		t.TargetSessionID, orDefault(string(t.OnRunCompleted), string(OnRunKeep)),
		orDefault(string(t.Multitask), string(MultitaskReject)), t.WebhookURL, t.EndTime, t.Enabled, t.NextRunAt, meta,
	)
	return scanTask(row)
}

// Get fetches one task by id.
func (s *PGStore) Get(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM scheduled_task WHERE id = $1`, id)
	return scanTask(row)
}

// Update applies mutable fields and recomputes NextRunAt.
func (s *PGStore) Update(ctx context.Context, t Task) (Task, error) {
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	next, err := t.NextAfter(time.Now())
	if err != nil {
		return Task{}, err
	}
	meta, err := json.Marshal(nonNilMeta(t.Metadata))
	if err != nil {
		return Task{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE scheduled_task SET
			agent_def_name = NULLIF($2,''), prompt = NULLIF($3,''), tool_whitelist = $4::text[],
			cron = $5, timezone = $6, target_session_id = NULLIF($7,'')::uuid,
			on_run_completed = $8, multitask_strategy = $9, webhook_url = NULLIF($10,''), end_time = $11,
			enabled = $12, next_run_at = $13, metadata = $14, updated_at = now()
		WHERE id = $1
		RETURNING `+taskCols,
		t.ID, t.AgentDefName, t.Prompt, formatTextArray(t.ToolWhitelist), t.Cron, tzOr(t.Timezone),
		t.TargetSessionID, orDefault(string(t.OnRunCompleted), string(OnRunKeep)),
		orDefault(string(t.Multitask), string(MultitaskReject)), t.WebhookURL, t.EndTime, t.Enabled, next, meta,
	)
	return scanTask(row)
}

// Delete removes a task.
func (s *PGStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_task WHERE id = $1`, id)
	if err != nil {
		if identity.IsMalformedID(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delete task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled toggles a task's enabled flag.
func (s *PGStore) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_task SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
	if err != nil {
		if identity.IsMalformedID(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set task enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListForUser returns the caller-visible tasks (own + team), newest first.
func (s *PGStore) ListForUser(ctx context.Context, userID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+taskCols+` FROM scheduled_task
		WHERE user_id = $1
		   OR team_id IN (SELECT team_id FROM team_memberships WHERE user_id = $1)
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ListDue returns the trigger's scan set at `now`. Rows whose next_run_at sits
// at or before the epoch are legacy never-firing tasks (pre-fix zero seeds of
// an un-fireable cron): they are filtered out so the trigger never scans — or
// worse, claims — a row whose NextAfter must fail.
func (s *PGStore) ListDue(ctx context.Context, now time.Time) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+taskCols+` FROM scheduled_task
		WHERE enabled AND next_run_at <= $1
		  AND next_run_at > '1970-01-01 00:00:00+00'
		  AND (end_time IS NULL OR end_time > $1)
		ORDER BY next_run_at`, now)
	if err != nil {
		return nil, fmt.Errorf("list due tasks: %w", err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// Claim advances a still-due task atomically (design D4). The WHERE re-checks
// due-ness so a racing instance that already advanced next_run_at (or an
// operator who disabled/expired the task between scan and claim) matches zero
// rows; we map that to ErrNotFound, which the trigger reads as "skip". The
// final `next_run_at = $4` guard closes the Get→UPDATE window: the next
// occurrence is computed from the row read above, and if an operator's edit
// (cron change → fresh next_run_at, or a requeue) landed between that read and
// this UPDATE, the stale computation must not clobber the new schedule.
func (s *PGStore) Claim(ctx context.Context, id string, now time.Time) (Task, error) {
	// Read the current schedule first to compute the next occurrence; the
	// UPDATE's WHERE guards the race, so a stale read here only costs a skipped
	// fire, never a double one.
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	next, err := cur.NextAfter(now)
	if err != nil {
		return Task{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE scheduled_task
		SET next_run_at = $2, last_run_at = $3, updated_at = now()
		WHERE id = $1 AND enabled AND next_run_at <= $3
		  AND (end_time IS NULL OR end_time > $3)
		  AND next_run_at = $4
		RETURNING `+taskCols, id, next, now, cur.NextRunAt)
	return scanTask(row)
}

// RequeueDue pushes a task's next_run_at back to `now` so the next scan picks
// it up again — the counterweight to Claim for pre-submit skips (design note:
// a skip must not burn the slot the claim consumed). The WHERE re-checks the
// claimed state: next_run_at must still be what Claim set (t.NextRunAt), and
// the task must still be enabled and unexpired — so a requeue can neither
// clobber a cron an operator edited between claim and requeue nor resurrect a
// task disabled/expired in that window. A zero-row update means the claim
// state no longer holds; ErrNotFound is fine there.
func (s *PGStore) RequeueDue(ctx context.Context, t Task, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_task SET next_run_at = $2, updated_at = now()
		 WHERE id = $1 AND enabled AND next_run_at = $3
		   AND (end_time IS NULL OR end_time > $2)`, t.ID, now, t.NextRunAt)
	if err != nil {
		if identity.IsMalformedID(err) {
			return ErrNotFound
		}
		return fmt.Errorf("requeue task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListSessions returns the ids of active sessions a task produced, newest
// first. Ended sessions are hidden (the same rule the sidebar applies), so a
// cleared task's list empties.
func (s *PGStore) ListSessions(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM sessions WHERE task_id = $1 AND status = $2 ORDER BY created_at DESC`,
		taskID, string(session.SessionActive))
	if err != nil {
		if identity.IsMalformedID(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("list task sessions: %w", err)
	}
	defer rows.Close()
	// Non-nil so the JSON form is [] rather than null — a client reading
	// sessions.length must not trip on a task that has fired nothing yet.
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListProducedSessions is ListSessions with the display columns (title, created
// time) the console needs to render each session by name. Same visibility rule:
// active only, newest first.
func (s *PGStore) ListProducedSessions(ctx context.Context, taskID string) ([]ProducedSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, created_at FROM sessions WHERE task_id = $1 AND status = $2 ORDER BY created_at DESC`,
		taskID, string(session.SessionActive))
	if err != nil {
		if identity.IsMalformedID(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("list task session infos: %w", err)
	}
	defer rows.Close()
	out := []ProducedSession{} // non-nil so JSON is []
	for rows.Next() {
		var ps ProducedSession
		if err := rows.Scan(&ps.ID, &ps.Title, &ps.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ps)
	}
	return out, rows.Err()
}

// EndSessions soft-deletes every active session a task produced, returning how
// many were cleared. Soft-delete (status → ended) matches the chat delete path:
// the rows — and their messages, runs, and approvals — stay for audit and
// dreaming; only the sidebar and ListSessions hide them. Restricting to active
// rows makes a repeat clear a no-op (0).
func (s *PGStore) EndSessions(ctx context.Context, taskID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET status = $2, ended_at = now(), updated_at = now()
		WHERE task_id = $1 AND status = $3`,
		taskID, string(session.SessionEnded), string(session.SessionActive))
	if err != nil {
		if identity.IsMalformedID(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("clear task sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// rowScanner abstracts QueryRowContext's Row and Rows for shared scanning.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTask scans one task row, mapping empty results and malformed ids to
// ErrNotFound.
func scanTask(row *sql.Row) (Task, error) {
	t, err := scanOneTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || identity.IsMalformedID(err) {
			return Task{}, ErrNotFound
		}
		return Task{}, err
	}
	return t, nil
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	// Non-nil so the JSON form is [] rather than null for a caller with no tasks.
	out := []Task{}
	for rows.Next() {
		t, err := scanOneTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanOneTask(row rowScanner) (Task, error) {
	var t Task
	var endTime, lastRun sql.NullTime
	var meta []byte
	var whitelist string
	err := row.Scan(
		&t.ID, &t.UserID, &t.TeamID, &t.AgentDefName, &t.Prompt, &whitelist,
		&t.Cron, &t.Timezone, &t.TargetSessionID, &t.OnRunCompleted, &t.Multitask,
		&t.WebhookURL,
		&endTime, &t.Enabled, &t.NextRunAt, &lastRun, &meta, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return Task{}, fmt.Errorf("scan task: %w", err)
	}
	t.ToolWhitelist = parseTextArray(whitelist)
	if endTime.Valid {
		t.EndTime = &endTime.Time
	}
	if lastRun.Valid {
		t.LastRunAt = &lastRun.Time
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &t.Metadata)
	}
	return t, nil
}

func nonNilMeta(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func tzOr(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// formatTextArray renders a Go slice as a Postgres text[] literal for the
// driver (the project uses pgx stdlib, which does not marshal []string into an
// array param). Elements are quoted and escaped; tool names are simple
// identifiers, so this stays minimal.
func formatTextArray(items []string) string {
	if len(items) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(it))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// parseTextArray reads a Postgres text[] literal back into a slice. The driver
// hands arrays to us as their text form; elements we write are quoted, so this
// strips the braces and unquotes. It tolerates the empty array and unquoted
// simple elements.
func parseTextArray(lit string) []string {
	lit = strings.TrimSpace(lit)
	if lit == "" || lit == "{}" {
		return nil
	}
	lit = strings.TrimPrefix(lit, "{")
	lit = strings.TrimSuffix(lit, "}")
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(lit); i++ {
		c := lit[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(lit):
			cur.WriteByte(lit[i+1])
			i++
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ---------------------------------------------------------------------------
// In-memory implementation
// ---------------------------------------------------------------------------

// MemStore is an in-memory Store for tests and dev without a database. It is
// single-process, so Claim needs no atomic guard — the scan/claim race the PG
// store defends against cannot occur here (design D4 note).
type MemStore struct {
	tasks map[string]Task
	// sessions maps task id -> session ids produced, mirroring the sessions
	// table's task_id back-reference for ListSessions.
	sessions map[string][]string
}

// NewMemStore creates an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{tasks: map[string]Task{}, sessions: map[string][]string{}}
}

func (m *MemStore) Create(ctx context.Context, t Task) (Task, error) {
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	next, err := t.NextAfter(time.Now())
	if err != nil {
		return Task{}, err
	}
	t.ID = uuid.NewString()
	t.NextRunAt = next
	t.CreatedAt = time.Now()
	t.UpdatedAt = t.CreatedAt
	if t.OnRunCompleted == "" {
		t.OnRunCompleted = OnRunKeep
	}
	if t.Multitask == "" {
		t.Multitask = MultitaskReject
	}
	if t.Timezone == "" {
		t.Timezone = "UTC"
	}
	m.tasks[t.ID] = t
	return t, nil
}

func (m *MemStore) Get(ctx context.Context, id string) (Task, error) {
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return t, nil
}

func (m *MemStore) Update(ctx context.Context, t Task) (Task, error) {
	if _, ok := m.tasks[t.ID]; !ok {
		return Task{}, ErrNotFound
	}
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	next, err := t.NextAfter(time.Now())
	if err != nil {
		return Task{}, err
	}
	cur := m.tasks[t.ID]
	t.NextRunAt = next
	t.CreatedAt = cur.CreatedAt
	t.UpdatedAt = time.Now()
	m.tasks[t.ID] = t
	return t, nil
}

func (m *MemStore) Delete(ctx context.Context, id string) error {
	if _, ok := m.tasks[id]; !ok {
		return ErrNotFound
	}
	delete(m.tasks, id)
	delete(m.sessions, id)
	return nil
}

func (m *MemStore) SetEnabled(ctx context.Context, id string, enabled bool) error {
	t, ok := m.tasks[id]
	if !ok {
		return ErrNotFound
	}
	t.Enabled = enabled
	t.UpdatedAt = time.Now()
	m.tasks[id] = t
	return nil
}

func (m *MemStore) ListForUser(ctx context.Context, userID string) ([]Task, error) {
	var out []Task
	for _, t := range m.tasks {
		if t.UserID == userID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *MemStore) ListDue(ctx context.Context, now time.Time) ([]Task, error) {
	var out []Task
	for _, t := range m.tasks {
		if t.Due(now) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *MemStore) Claim(ctx context.Context, id string, now time.Time) (Task, error) {
	t, ok := m.tasks[id]
	if !ok || !t.Due(now) {
		return Task{}, ErrNotFound
	}
	next, err := t.NextAfter(now)
	if err != nil {
		return Task{}, err
	}
	t.LastRunAt = &now
	t.NextRunAt = next
	t.UpdatedAt = now
	m.tasks[id] = t
	return t, nil
}

// RequeueDue pushes the task's NextRunAt back to `now` (MemStore mirror of the
// PG skip-path counterweight to Claim). Like the PG store, it guards on the
// claimed state: the stored NextRunAt must still match t.NextRunAt and the
// task must still be enabled and unexpired, so an edit between claim and
// requeue makes it a no-op (ErrNotFound) rather than a clobber.
func (m *MemStore) RequeueDue(ctx context.Context, t Task, now time.Time) error {
	cur, ok := m.tasks[t.ID]
	if !ok || !cur.Enabled || !cur.NextRunAt.Equal(t.NextRunAt) {
		return ErrNotFound
	}
	if cur.EndTime != nil && !cur.EndTime.After(now) {
		return ErrNotFound
	}
	cur.NextRunAt = now
	cur.UpdatedAt = now
	m.tasks[t.ID] = cur
	return nil
}

func (m *MemStore) ListSessions(ctx context.Context, taskID string) ([]string, error) {
	// Copy + non-nil, matching the PG contract (JSON [] not null).
	ids := m.sessions[taskID]
	out := make([]string, len(ids))
	copy(out, ids)
	return out, nil
}

// EndSessions clears the task's produced-session list, returning how many were
// dropped. The in-memory store has no status column, so clearing removes the
// ids outright — equivalent to the PG soft-delete as far as ListSessions sees.
func (m *MemStore) EndSessions(ctx context.Context, taskID string) (int, error) {
	n := len(m.sessions[taskID])
	delete(m.sessions, taskID)
	return n, nil
}

// RecordSession associates a produced session with a task (test helper mirroring
// the sessions.task_id back-reference; the PG path reads the sessions table).
func (m *MemStore) RecordSession(taskID, sessionID string) {
	m.sessions[taskID] = append([]string{sessionID}, m.sessions[taskID]...)
}
