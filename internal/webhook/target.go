package webhook

import (
	"context"
	"database/sql"
)

// TargetResolver resolves the notification target URL for a run's session:
// the run's scheduled-task webhook_url wins, then the inbound webhook's
// notify_url for inbound-originated runs, then the global URL. Runs with no
// URL anywhere stay silent (Resolve returns "").
type TargetResolver struct {
	db *sql.DB
	// globalURL supplies the fallback target and is called per resolution, so
	// a runtime-setting change (admin-console webhook_url) applies to the next
	// notification without a restart.
	globalURL func() string
}

// NewTargetResolver builds a TargetResolver over the gateway database.
func NewTargetResolver(db *sql.DB, globalURL func() string) *TargetResolver {
	return &TargetResolver{db: db, globalURL: globalURL}
}

// Resolve returns the notification URL for sessionID, or "" when the run has
// none. A failure to read the session row errors; a hiccup on the per-run
// tables degrades to the global URL rather than failing the notification.
func (r *TargetResolver) Resolve(ctx context.Context, sessionID string) (string, error) {
	var taskID sql.NullString
	var source sql.NullString
	var webhookID string
	if err := r.db.QueryRowContext(ctx,
		`SELECT task_id, source, COALESCE(metadata->>'webhook_id','') FROM sessions WHERE id = $1`, sessionID).
		Scan(&taskID, &source, &webhookID); err != nil {
		return "", err
	}
	// A scheduled-task run notifies its task's webhook_url when set.
	if taskID.Valid && taskID.String != "" {
		var u sql.NullString
		if err := r.db.QueryRowContext(ctx, `SELECT webhook_url FROM scheduled_task WHERE id = $1`, taskID.String).Scan(&u); err == nil && u.Valid && u.String != "" {
			return u.String, nil
		}
	}
	// An inbound-webhook run (sessions.source = 'inbound') notifies its own
	// notify_url when set, before falling back to the global URL.
	if source.String == "inbound" && webhookID != "" {
		var u sql.NullString
		if err := r.db.QueryRowContext(ctx, `SELECT notify_url FROM inbound_webhooks WHERE id = $1`, webhookID).Scan(&u); err == nil && u.Valid && u.String != "" {
			return u.String, nil
		}
	}
	// Global target: read live from the runtime settings (admin console can
	// retarget all notifications without a restart).
	if r.globalURL != nil {
		return r.globalURL(), nil
	}
	return "", nil
}
