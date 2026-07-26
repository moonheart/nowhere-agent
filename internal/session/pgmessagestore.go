package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO messages (session_id, run_id, seq, role, content,
			usage_input, usage_output, usage_cache_read, usage_cache_write)
		VALUES ($1, $2,
			(SELECT COALESCE(MAX(seq), -1) + 1 FROM messages WHERE session_id = $1),
			$3, $4, $5, $6, $7, $8)
		RETURNING id, seq, created_at`,
		msg.SessionID, msg.RunID, string(msg.Role), content, ui, uo, ucr, ucw,
	).Scan(&msg.ID, &msg.Seq, &msg.CreatedAt)
	if err != nil {
		return StoredMessage{}, fmt.Errorf("append message: %w", err)
	}
	return msg, nil
}

// MessagesFor returns a session's messages ordered by seq (full conversation).
func (s *PGMessageStore) MessagesFor(ctx context.Context, sessionID string) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, run_id, seq, role, content, created_at,
			usage_input, usage_output, usage_cache_read, usage_cache_write
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
			usage_input, usage_output, usage_cache_read, usage_cache_write
		FROM messages
		WHERE session_id = $1 AND id > $2
		ORDER BY seq`, sessionID, afterID)
	if err != nil {
		return nil, fmt.Errorf("messages after: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// scanMessages reads message rows (id, session_id, run_id, seq, role, content,
// created_at, usage cols) into StoredMessages. Usage is rebuilt when the row
// recorded it (usage_input non-NULL); otherwise it stays nil.
func scanMessages(rows *sql.Rows) ([]StoredMessage, error) {
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var role string
		var content []byte
		var ui, uo, ucr, ucw sql.NullInt64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.RunID, &m.Seq, &role, &content, &m.CreatedAt, &ui, &uo, &ucr, &ucw); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Role = provider.Role(role)
		if err := json.Unmarshal(content, &m.Content); err != nil {
			return nil, fmt.Errorf("unmarshal content: %w", err)
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
