// Package export assembles a user's full data footprint into one JSON document
// (data governance / GDPR-style export): profile, sessions with their complete
// message history, memories, uploads, and scheduled tasks. It is the "give me
// my data" surface an enterprise asks about — read-only, self-service, and
// confined to the requesting user's own rows.
package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/schedule"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/upload"
)

// Tasks returns the caller-visible scheduled tasks (own + team). *schedule
// PGStore satisfies it via ListForUser.
type Tasks interface {
	ListForUser(ctx context.Context, userID string) ([]schedule.Task, error)
}

// Service assembles one user's data footprint.
type Service struct {
	db      *sql.DB
	msgs    session.MessageStore
	mem     memory.Port
	uploads upload.Uploader
	tasks   Tasks
}

// New builds the export service. msgs/mem/uploads/tasks may be nil — the
// corresponding section is then omitted from the document (a deployment
// without the memory port still exports the conversation record).
func New(db *sql.DB, msgs session.MessageStore, mem memory.Port, up upload.Uploader, tasks Tasks) *Service {
	return &Service{db: db, msgs: msgs, mem: mem, uploads: up, tasks: tasks}
}

// sessionMeta is one exported session WITHOUT its messages — the message array
// is streamed separately, page by page, so a session's full history never
// materializes in memory.
type sessionMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// messageRow is the wire form of one conversation message (role, blocks, time).
// Content blocks carry the full original shape the UI renders from.
type messageRow struct {
	ID        int64            `json:"id"`
	RunID     string           `json:"run_id,omitempty"`
	Seq       int              `json:"seq"`
	Role      string           `json:"role"`
	Content   []provider.Block `json:"content"`
	CreatedAt time.Time        `json:"created_at"`
}

// memoryRow is one exported memory (no embedding vector — it is a retrieval
// artifact, not user data).
type memoryRow struct {
	ID         string    `json:"id"`
	Scope      string    `json:"scope"`
	Kind       string    `json:"kind"`
	Content    string    `json:"content"`
	Deprecated bool      `json:"deprecated"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// taskRow is one exported scheduled task (mirrors the console DTO shape).
type taskRow struct {
	ID              string    `json:"id"`
	AgentDefName    string    `json:"agent_def_name,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`
	Cron            string    `json:"cron"`
	Timezone        string    `json:"timezone"`
	TargetSessionID string    `json:"target_session_id,omitempty"`
	OnRunCompleted  string    `json:"on_run_completed"`
	Multitask       string    `json:"multitask_strategy"`
	WebhookURL      string    `json:"webhook_url,omitempty"`
	Enabled         bool      `json:"enabled"`
	NextRunAt       time.Time `json:"next_run_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// sessionsForUser loads the METADATA of every session of the user (active AND
// ended — an export is a complete copy, not a sidebar). Messages are not part
// of it: they are streamed per session via writeSession, so the memory
// footprint is one session's page, not the user's whole history. A nil db
// (tests) yields no rows.
func (s *Service) sessionsForUser(ctx context.Context, userID string) ([]sessionMeta, error) {
	if s.db == nil {
		return []sessionMeta{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, status, created_at, updated_at FROM sessions
		WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("export sessions: %w", err)
	}
	defer rows.Close()
	out := []sessionMeta{}
	for rows.Next() {
		var sr sessionMeta
		if err := rows.Scan(&sr.ID, &sr.Title, &sr.Status, &sr.CreatedAt, &sr.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// messagePageSize bounds the working set of one keyset page of a session's
// messages. Export dumps a user's whole conversation record; paging keeps the
// stream's memory proportional to a page rather than to the record.
const messagePageSize = 500

// writeSession emits one session object: its metadata header fields, then its
// messages streamed in keyset pages (MessagesPage: id > last, ordered by seq).
// The JSON shape is unchanged from the old whole-session encoding — a session
// without messages renders "messages":null exactly as before.
func (s *Service) writeSession(ctx context.Context, w io.Writer, enc *json.Encoder, sr sessionMeta) error {
	head, err := json.Marshal(sr)
	if err != nil {
		return err
	}
	// The marshaled object ends with "}", and the message array is spliced in
	// BEFORE that brace — drop it here and write it after the last page.
	if _, err := w.Write(head[:len(head)-1]); err != nil {
		return err
	}
	if s.msgs == nil {
		_, err = io.WriteString(w, `,"messages":null}`)
		return err
	}

	page, err := s.msgs.MessagesPage(ctx, sr.ID, 0, messagePageSize)
	if err != nil {
		return fmt.Errorf("export messages: %w", err)
	}
	if len(page) == 0 {
		if _, err := io.WriteString(w, `,"messages":null}`); err != nil {
			return err
		}
		return nil
	}
	if _, err := io.WriteString(w, `,"messages":[`); err != nil {
		return err
	}
	// Rows need EXPLICIT separators: Go 1.26's strict json parser rejects
	// whitespace-only element separation (RFC 8259 requires the comma).
	first := true
	writeRow := func(m session.StoredMessage) error {
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		return enc.Encode(messageRow{
			ID:        m.ID,
			RunID:     m.RunID,
			Seq:       m.Seq,
			Role:      string(m.Role),
			Content:   m.Content,
			CreatedAt: m.CreatedAt,
		})
	}
	for {
		for _, m := range page {
			if err := writeRow(m); err != nil {
				return err
			}
		}
		// A short page is the last one; a full page may have a successor (the
		// next query returns empty when it does not).
		if len(page) < messagePageSize {
			break
		}
		page, err = s.msgs.MessagesPage(ctx, sr.ID, page[len(page)-1].ID, messagePageSize)
		if err != nil {
			return fmt.Errorf("export messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
	}
	_, err = io.WriteString(w, `]}`)
	return err
}

// Write writes the export document for userID to w as JSON. The document is
// ENCODED incrementally: sessions are flushed one at a time, and each session's
// messages are streamed in keyset pages (MessagesPage), so memory holds one
// page of one session rather than the user's entire conversation history —
// the same streaming posture as the SSE and history paths. The identity rows
// are provided by the caller (the authenticated request context already
// holds them; the export is a copy, not a re-query).
func (s *Service) Write(ctx context.Context, w io.Writer, u identity.User) error {
	enc := json.NewEncoder(w)
	// Open the object: header fields first. The user object's own braces come
	// from the encoder — the prefix only names the key.
	if _, err := fmt.Fprintf(w, `{"exported_at":%q,"user":`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := enc.Encode(map[string]any{
		"id":            u.ID,
		"email":         u.Email,
		"display_name":  u.DisplayName,
		"platform_role": string(u.PlatformRole),
		"created_at":    u.CreatedAt,
	}); err != nil {
		return err
	}
	// Trim the trailing newline the encoder wrote; continue the object.
	if _, err := io.WriteString(w, `,"sessions":[`); err != nil {
		return err
	}

	sessions, err := s.sessionsForUser(ctx, u.ID)
	if err != nil {
		return err
	}
	for i, sr := range sessions {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := s.writeSession(ctx, w, enc, sr); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, `]`); err != nil {
		return err
	}

	// Memories (user scope).
	if s.mem != nil {
		mems, err := s.mem.ListByScope(ctx, identity.UserScope(u.ID))
		if err != nil {
			return fmt.Errorf("export memories: %w", err)
		}
		rows := make([]memoryRow, 0, len(mems))
		for _, m := range mems {
			rows = append(rows, memoryRow{
				ID:         m.ID,
				Scope:      string(m.Scope.Scope),
				Kind:       string(m.Kind),
				Content:    m.Content,
				Deprecated: m.Deprecated,
				CreatedAt:  m.CreatedAt,
				UpdatedAt:  m.UpdatedAt,
			})
		}
		if _, err := io.WriteString(w, `,"memories":`); err != nil {
			return err
		}
		if err := enc.Encode(rows); err != nil {
			return err
		}
	}

	// Uploads (metadata + blob pointers; the blobs themselves are served by
	// the existing upload endpoints).
	if s.uploads != nil {
		ups, err := s.uploads.List(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("export uploads: %w", err)
		}
		if _, err := io.WriteString(w, `,"uploads":`); err != nil {
			return err
		}
		if err := enc.Encode(ups); err != nil {
			return err
		}
	}

	// Scheduled tasks the user owns.
	if s.tasks != nil {
		tasks, err := s.tasks.ListForUser(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("export tasks: %w", err)
		}
		rows := make([]taskRow, 0, len(tasks))
		for _, t := range tasks {
			rows = append(rows, taskRow{
				ID:              t.ID,
				AgentDefName:    t.AgentDefName,
				Prompt:          t.Prompt,
				Cron:            t.Cron,
				Timezone:        t.Timezone,
				TargetSessionID: t.TargetSessionID,
				OnRunCompleted:  string(t.OnRunCompleted),
				Multitask:       string(t.Multitask),
				WebhookURL:      t.WebhookURL,
				Enabled:         t.Enabled,
				NextRunAt:       t.NextRunAt,
				CreatedAt:       t.CreatedAt,
				UpdatedAt:       t.UpdatedAt,
			})
		}
		if _, err := io.WriteString(w, `,"scheduled_tasks":`); err != nil {
			return err
		}
		if err := enc.Encode(rows); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(w, "}\n"); err != nil {
		return err
	}
	return nil
}
