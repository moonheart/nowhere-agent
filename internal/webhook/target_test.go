package webhook

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestTargetResolverPrecedence pins the notification-target resolution order:
// the run's scheduled-task webhook_url wins, then the inbound webhook's
// notify_url, then the global URL; a run with no URL anywhere stays silent.
// All seeded rows are the test's own and deleted by ID on cleanup.
func TestTargetResolverPrecedence(t *testing.T) {
	db := outboxTestDB(t)
	ctx := context.Background()
	r := NewTargetResolver(db, func() string { return "https://global.example.com/hook" })

	var userID string
	if err := db.QueryRow(
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		"tgt-"+time.Now().Format("150405.000000000")+"@test.dev").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	// A scheduled-task run notifies the task's webhook_url.
	var taskID string
	if err := db.QueryRow(`
		INSERT INTO scheduled_task (user_id, cron, timezone, next_run_at, prompt, webhook_url)
		VALUES ($1, '0 0 * * *', 'UTC', now(), 'run', 'https://task.example.com/hook') RETURNING id`,
		userID).Scan(&taskID); err != nil {
		t.Fatalf("seed scheduled_task: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM scheduled_task WHERE id = $1`, taskID) })

	taskSession := seedResolverSession(t, db, userID, "scheduled", &taskID, "")
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, taskSession) })
	if got, err := r.Resolve(ctx, taskSession); err != nil || got != "https://task.example.com/hook" {
		t.Fatalf("task session: got %q err %v, want the task URL", got, err)
	}

	// A task with no webhook_url falls back to the global URL.
	var bareTaskID string
	if err := db.QueryRow(`
		INSERT INTO scheduled_task (user_id, cron, timezone, next_run_at, prompt)
		VALUES ($1, '0 0 * * *', 'UTC', now(), 'run') RETURNING id`,
		userID).Scan(&bareTaskID); err != nil {
		t.Fatalf("seed bare scheduled_task: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM scheduled_task WHERE id = $1`, bareTaskID) })
	bareSession := seedResolverSession(t, db, userID, "scheduled", &bareTaskID, "")
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, bareSession) })
	if got, err := r.Resolve(ctx, bareSession); err != nil || got != "https://global.example.com/hook" {
		t.Fatalf("bare task session: got %q err %v, want the global URL", got, err)
	}

	// An inbound-webhook run notifies its own notify_url.
	var webhookID string
	if err := db.QueryRow(`
		INSERT INTO inbound_webhooks (name, user_id, secret_cipher, notify_url)
		VALUES ('t', $1, 'cipher', 'https://inbound.example.com/notify') RETURNING id`,
		userID).Scan(&webhookID); err != nil {
		t.Fatalf("seed inbound_webhooks: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM inbound_webhooks WHERE id = $1`, webhookID) })
	inboundSession := seedResolverSession(t, db, userID, "inbound", nil, `{"webhook_id":"`+webhookID+`"}`)
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, inboundSession) })
	if got, err := r.Resolve(ctx, inboundSession); err != nil || got != "https://inbound.example.com/notify" {
		t.Fatalf("inbound session: got %q err %v, want the notify_url", got, err)
	}

	// A plain chat run resolves to the global URL.
	plainSession := seedResolverSession(t, db, userID, "human", nil, "")
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, plainSession) })
	if got, err := r.Resolve(ctx, plainSession); err != nil || got != "https://global.example.com/hook" {
		t.Fatalf("plain session: got %q err %v, want the global URL", got, err)
	}

	// An unknown session errors instead of silently resolving to "".
	if _, err := r.Resolve(ctx, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("unknown session: want an error")
	}
}

func seedResolverSession(t *testing.T, db *sql.DB, userID, source string, taskID *string, metadata string) string {
	t.Helper()
	if metadata == "" {
		metadata = "{}"
	}
	var id string
	if err := db.QueryRow(`
		INSERT INTO sessions (user_id, source, task_id, metadata)
		VALUES ($1, $2, $3, $4::jsonb) RETURNING id`,
		userID, source, taskID, metadata).Scan(&id); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}
