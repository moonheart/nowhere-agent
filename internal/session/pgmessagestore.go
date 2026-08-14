package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"nowhere-agent/internal/provider"
)

// PGMessageStore persists conversation messages in Postgres. Content is stored
// as a JSONB block array; seq is assigned per-session by continuing from the
// durable max so appends stay monotonic across runs and after mid-run settles.
type PGMessageStore struct {
	db *sql.DB
}

// NewPGMessageStore creates a Postgres-backed MessageStore.
func NewPGMessageStore(db *sql.DB) *PGMessageStore { return &PGMessageStore{db: db} }

// AppendMessage persists one message at the next per-session seq.
func (s *PGMessageStore) AppendMessage(ctx context.Context, msg StoredMessage) (StoredMessage, error) {
	content, err := json.Marshal(msg.Content)
	if err != nil {
		return StoredMessage{}, fmt.Errorf("marshal content: %w", err)
	}
	// nil content must not become SQL NULL (content is NOT NULL); encode as [].
	if msg.Content == nil {
		content = []byte("[]")
	}

	// Usage cols stay NULL for messages with no reported usage (user/tool_result).
	var ui, uo, ucr, ucw sql.NullInt64
	if msg.Usage != nil {
		ui = sql.NullInt64{Int64: int64(msg.Usage.InputTokens), Valid: true}
		uo = sql.NullInt64{Int64: int64(msg.Usage.OutputTokens), Valid: true}
		ucr = sql.NullInt64{Int64: int64(msg.Usage.CacheReadTokens), Valid: true}
		ucw = sql.NullInt64{Int64: int64(msg.Usage.CacheWriteTokens), Valid: true}
	}
	// Metadata stays NULL for messages with none.
	var meta sql.NullString
	if len(msg.Metadata) > 0 {
		meta = sql.NullString{String: string(msg.Metadata), Valid: true}
	}

	// A positive msg.ID is a pre-provisioned id (a run_steps intent reserved it
	// via nextval); the INSERT then uses it explicitly so the intent's binding
	// and the message row agree. Zero lets the default BIGSERIAL assign.
	if msg.ID > 0 {
		err = s.db.QueryRowContext(ctx, `
			INSERT INTO messages (id, session_id, run_id, seq, role, content,
				usage_input, usage_output, usage_cache_read, usage_cache_write, metadata)
			VALUES ($1, $2, $3,
				(SELECT COALESCE(MAX(seq), -1) + 1 FROM messages WHERE session_id = $2),
				$4, $5, $6, $7, $8, $9, $10)
			RETURNING id, seq, created_at`,
			msg.ID, msg.SessionID, msg.RunID, string(msg.Role), content, ui, uo, ucr, ucw, meta,
		).Scan(&msg.ID, &msg.Seq, &msg.CreatedAt)
		if err != nil {
			return StoredMessage{}, fmt.Errorf("append message: %w", err)
		}
		return msg, nil
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO messages (session_id, run_id, seq, role, content,
			usage_input, usage_output, usage_cache_read, usage_cache_write, metadata)
		VALUES ($1, $2,
			(SELECT COALESCE(MAX(seq), -1) + 1 FROM messages WHERE session_id = $1),
			$3, $4, $5, $6, $7, $8, $9)
		RETURNING id, seq, created_at`,
		msg.SessionID, msg.RunID, string(msg.Role), content, ui, uo, ucr, ucw, meta,
	).Scan(&msg.ID, &msg.Seq, &msg.CreatedAt)
	if err != nil {
		return StoredMessage{}, fmt.Errorf("append message: %w", err)
	}
	return msg, nil
}

// MessagesFor returns a session's messages ordered by seq (full conversation).
// The read is deliberately UNBOUNDED — every caller needs the whole record:
// the chat resume path rebuilds the loop's provider history from it (with
// in-loop compression applying afterwards, near the context window), FoldBatch
// must resolve tool_use/tool_result pairs across the entire conversation for
// correctness, and export dumps everything. Bounding it would silently drop
// older turns from any of those paths. The one display-only consumer
// (GET /api/chat/history) also reads it fully: the web client's history.load()
// has no truncation semantics, so a bounded page would render an incomplete
// conversation as complete — the frontend contract wins over memory here. A
// bounded variant would be a new interface method (following LastAssistantText)
// plus a client-side truncation signal; revisit when the frontend grows one.
func (s *PGMessageStore) MessagesFor(ctx context.Context, sessionID string) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, run_id, seq, role, content, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write, metadata
		FROM messages
		WHERE session_id = $1
		ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("messages for: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// MessagesAfter returns a session's messages with id > afterID, ordered by seq.
func (s *PGMessageStore) MessagesAfter(ctx context.Context, sessionID string, afterID int64) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, run_id, seq, role, content, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write, metadata
		FROM messages
		WHERE session_id = $1 AND id > $2
		ORDER BY seq`, sessionID, afterID)
	if err != nil {
		return nil, fmt.Errorf("messages after: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// MessagesPage returns up to limit messages with id > afterID, ordered by seq
// (see MessageStore.MessagesPage). The keyset cursor is the message id: ids
// ascend with seq (append-only, assigned by nextval in seq order), so paging by
// id advances through the conversation in the same order MessagesFor renders.
func (s *PGMessageStore) MessagesPage(ctx context.Context, sessionID string, afterID int64, limit int) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, run_id, seq, role, content, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write, metadata
		FROM messages
		WHERE session_id = $1 AND id > $2
		ORDER BY seq
		LIMIT $3`, sessionID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("messages page: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// LastAssistantMessage returns the run's most recent assistant message (see
// MessageStore.LastAssistantMessage). The query is bounded to the run's
// assistant rows and ordered newest-first, so the cost is at most one row
// regardless of conversation length.
func (s *PGMessageStore) LastAssistantMessage(ctx context.Context, sessionID, runID string) (*StoredMessage, error) {
	var m StoredMessage
	var role string
	var content []byte
	var ui, uo, ucr, ucw sql.NullInt64
	var metadata []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, run_id, seq, role, content, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write, metadata
		FROM messages
		WHERE session_id = $1 AND run_id = $2 AND role = 'assistant'
		ORDER BY seq DESC
		LIMIT 1`, sessionID, runID).
		Scan(&m.ID, &m.SessionID, &m.RunID, &m.Seq, &role, &content, &m.CreatedAt, &ui, &uo, &ucr, &ucw, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("last assistant message: %w", err)
	}
	m.Role = provider.Role(role)
	if err := json.Unmarshal(content, &m.Content); err != nil {
		return nil, fmt.Errorf("last assistant message: %w", err)
	}
	if len(metadata) > 0 {
		m.Metadata = json.RawMessage(metadata)
	}
	if ui.Valid {
		m.Usage = &provider.Usage{
			InputTokens:      int(ui.Int64),
			OutputTokens:     int(uo.Int64),
			CacheReadTokens:  int(ucr.Int64),
			CacheWriteTokens: int(ucw.Int64),
		}
	}
	return &m, nil
}

// MessagesTail returns up to limit messages with id < beforeID, ordered by
// seq ascending (see MessageStore.MessagesTail): the newest messages older
// than the cursor, for the history tail page. Bounded read — the cost is
// proportional to limit, never the conversation length.
func (s *PGMessageStore) MessagesTail(ctx context.Context, sessionID string, beforeID int64, limit int) ([]StoredMessage, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, run_id, seq, role, content, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write, metadata
		FROM messages
		WHERE session_id = $1 AND ($2 <= 0 OR id < $2)
		ORDER BY seq DESC
		LIMIT $3`, sessionID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("messages tail: %w", err)
	}
	defer rows.Close()
	stored, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	// scanMessages yields newest-first; flip to seq order for the caller.
	slices.Reverse(stored)
	return stored, nil
}

// SetMessageMetadata replaces one message's metadata JSON. The row is keyed by
// id; a missing row is not an error (the update raced a delete), matching the
// best-effort contract.
func (s *PGMessageStore) SetMessageMetadata(ctx context.Context, id int64, metadata json.RawMessage) error {
	var meta sql.NullString
	if len(metadata) > 0 {
		meta = sql.NullString{String: string(metadata), Valid: true}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE messages SET metadata = $2 WHERE id = $1`, id, meta); err != nil {
		return fmt.Errorf("set message metadata: %w", err)
	}
	return nil
}

// LastAssistantText returns the most recent assistant text (see
// MessageStore.LastAssistantText). The query is bounded to the assistant rows
// only and ordered newest-first, so the cost is proportional to `limit`, not
// to the conversation length.
func (s *PGMessageStore) LastAssistantText(ctx context.Context, sessionID string, limit int) (string, error) {
	if limit <= 0 {
		return "", nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT content FROM messages
		WHERE session_id = $1 AND role = 'assistant'
		ORDER BY seq DESC
		LIMIT $2`, sessionID, limit)
	if err != nil {
		return "", fmt.Errorf("last assistant text: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return "", fmt.Errorf("last assistant text: %w", err)
		}
		var blocks []provider.Block
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", fmt.Errorf("last assistant text: %w", err)
		}
		if s := assistantText(blocks); s != "" {
			return s, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("last assistant text: %w", err)
	}
	return "", nil
}

// scanMessages reads message rows (id, session_id, run_id, seq, role, content,
// created_at, usage cols, metadata) into StoredMessages. Usage is rebuilt when
// the row recorded it (usage_input non-NULL); otherwise it stays nil.
func scanMessages(rows *sql.Rows) ([]StoredMessage, error) {
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var role string
		var content []byte
		var ui, uo, ucr, ucw sql.NullInt64
		var metadata []byte
		if err := rows.Scan(&m.ID, &m.SessionID, &m.RunID, &m.Seq, &role, &content, &m.CreatedAt, &ui, &uo, &ucr, &ucw, &metadata); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Role = provider.Role(role)
		if err := json.Unmarshal(content, &m.Content); err != nil {
			return nil, fmt.Errorf("unmarshal content: %w", err)
		}
		if len(metadata) > 0 {
			m.Metadata = json.RawMessage(metadata)
		}
		if ui.Valid {
			m.Usage = &provider.Usage{
				InputTokens:      int(ui.Int64),
				OutputTokens:     int(uo.Int64),
				CacheReadTokens:  int(ucr.Int64),
				CacheWriteTokens: int(ucw.Int64),
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

var _ MessageStore = (*PGMessageStore)(nil)
