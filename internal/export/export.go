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

// sessionRow is one exported session with its messages.
type sessionRow struct {
	ID        string              `json:"id"`
	Title     string              `json:"title"`
	Status    string              `json:"status"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	Messages  []messageRow        `json:"messages"`
}

// messageRow is the wire form of one conversation message (role, blocks, time).
// Content blocks carry the full original shape the UI renders from.
type messageRow struct {
	ID        int64             `json:"id"`
	RunID     string            `json:"run_id,omitempty"`
	Seq       int               `json:"seq"`
	Role      string            `json:"role"`
	Content   []provider.Block  `json:"content"`
	CreatedAt time.Time         `json:"created_at"`
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
	ID               string    `json:"id"`
	AgentDefName     string    `json:"agent_def_name,omitempty"`
	Prompt           string    `json:"prompt,omitempty"`
	Cron             string    `json:"cron"`
	Timezone         string    `json:"timezone"`
	TargetSessionID  string    `json:"target_session_id,omitempty"`
	OnRunCompleted   string    `json:"on_run_completed"`
	Multitask        string    `json:"multitask_strategy"`
	WebhookURL       string    `json:"webhook_url,omitempty"`
	Enabled          bool      `json:"enabled"`
	NextRunAt        time.Time `json:"next_run_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// sessionsForUser loads every session of the user (active AND ended — an
// export is a complete copy, not a sidebar). A nil db (tests) yields no rows.
func (s *Service) sessionsForUser(ctx context.Context, userID string) ([]sessionRow, error) {
	if s.db == nil {
		return []sessionRow{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, status, created_at, updated_at FROM sessions
		WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("export sessions: %w", err)
	}
	defer rows.Close()
	out := []sessionRow{}
	for rows.Next() {
		var sr sessionRow
		if err := rows.Scan(&sr.ID, &sr.Title, &sr.Status, &sr.CreatedAt, &sr.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if s.msgs != nil {
		for i := range out {
			msgs, err := s.msgs.MessagesFor(ctx, out[i].ID)
			if err != nil {
				return nil, err
			}
			for _, m := range msgs {
				out[i].Messages = append(out[i].Messages, messageRow{
					ID:        m.ID,
					RunID:     m.RunID,
					Seq:       m.Seq,
					Role:      string(m.Role),
					Content:   m.Content,
					CreatedAt: m.CreatedAt,
				})
			}
		}
	}
	return out, nil
}

// Write writes the export document for userID to w as JSON. The document is
// ENCODED incrementally (sessions are flushed to w one at a time), but the
// data itself is not streamed: sessionsForUser first loads every session and
// every message of the user into memory, so a very large conversation
// history is held in memory for the duration of the export. The identity
// rows are provided by the caller (the authenticated request context already
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
		if err := enc.Encode(sr); err != nil {
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
