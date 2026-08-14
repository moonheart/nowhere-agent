// Package inbound implements the inbound-webhook trigger surface: a per-user
// endpoint external systems can POST to so an agent run starts without an
// interactive client. It is the ingress counterpart of the outbound webhook
// notifier — ERP/OA/IM events hit /api/inbound/{id}, the dispatcher submits the
// run through the same RunRegistry a human chat uses, and completion flows
// back through the shared RunDoneHook.
//
// Authentication: each webhook has a random secret, returned once at creation
// and stored AES-256-GCM encrypted at rest (internal/secrets, the same
// protection team provider keys get). Trigger requests must present
// X-Nowhere-Timestamp (unix seconds, within a 5-minute window) and
// X-Nowhere-Signature: sha256=<hex HMAC-SHA256 over "<ts>.<nonce>.<body>">, so
// the secret never rides in a plain header, replays are bounded, and each
// signed event can only start one run.
package inbound

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"nowhere-agent/internal/secrets"
)

// ErrNotFound is returned when a webhook id matches no row.
var ErrNotFound = errors.New("inbound webhook not found")

// Webhook is one inbound trigger endpoint owned by a user.
type Webhook struct {
	ID              string
	Name            string
	UserID          string
	TeamID          string
	Secret          string // encrypted at rest; never serialize
	AgentDef        string
	SystemPrompt    string
	TargetSessionID string
	NotifyURL       string
	Enabled         bool
	LastUsedAt      *time.Time
	CreatedAt       time.Time
}

// Store persists inbound webhooks.
type Store struct {
	db  *sql.DB
	enc *secrets.Encryptor
}

// NewStore creates a Store over a database handle.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// WithEncryption enables encryption-at-rest for webhook secrets (the same
// Encryptor the routing layer uses for team provider keys). Nil leaves secrets
// in legacy plaintext — the decrypt path reads both forms, so enabling
// encryption is not a flag day.
func (s *Store) WithEncryption(enc *secrets.Encryptor) *Store {
	s.enc = enc
	return s
}

const webhookCols = `id, name, user_id, team_id, secret_cipher, agent_def,
	system_prompt, target_session_id, notify_url, enabled, last_used_at, created_at`

func (s *Store) scanWebhook(row interface{ Scan(...any) error }) (Webhook, error) {
	var w Webhook
	var teamID, agentDef, systemPrompt, targetSessionID, notifyURL sql.NullString
	err := row.Scan(&w.ID, &w.Name, &w.UserID, &teamID, &w.Secret,
		&agentDef, &systemPrompt, &targetSessionID, &notifyURL,
		&w.Enabled, &w.LastUsedAt, &w.CreatedAt)
	w.TeamID, w.AgentDef, w.SystemPrompt = teamID.String, agentDef.String, systemPrompt.String
	w.TargetSessionID, w.NotifyURL = targetSessionID.String, notifyURL.String
	if err != nil {
		return w, err
	}
	if s.enc != nil {
		plain, derr := s.enc.Decrypt(w.Secret)
		if derr != nil {
			return w, derr
		}
		w.Secret = plain
	}
	return w, nil
}

// Create inserts a webhook and returns it.
func (s *Store) Create(ctx context.Context, w Webhook) (Webhook, error) {
	secret := w.Secret
	if s.enc != nil {
		enc, err := s.enc.Encrypt(secret)
		if err != nil {
			return Webhook{}, err
		}
		secret = enc
	}
	var teamID, agentDef, systemPrompt, targetSessionID, notifyURL sql.NullString
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO inbound_webhooks
			(name, user_id, team_id, secret_cipher, agent_def, system_prompt,
			 target_session_id, notify_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+webhookCols,
		w.Name, w.UserID, nullIfEmpty(w.TeamID), secret,
		nullIfEmpty(w.AgentDef), nullIfEmpty(w.SystemPrompt),
		nullIfEmpty(w.TargetSessionID), nullIfEmpty(w.NotifyURL)).Scan(
		&w.ID, &w.Name, &w.UserID, &teamID, &w.Secret,
		&agentDef, &systemPrompt, &targetSessionID, &notifyURL,
		&w.Enabled, &w.LastUsedAt, &w.CreatedAt)
	w.TeamID, w.AgentDef, w.SystemPrompt = teamID.String, agentDef.String, systemPrompt.String
	w.TargetSessionID, w.NotifyURL = targetSessionID.String, notifyURL.String
	if err != nil {
		return Webhook{}, err
	}
	w.Secret = secret
	return w, nil
}

// GetByID loads a webhook by id — the trigger path, which authenticates by
// signature, not by user session.
func (s *Store) GetByID(ctx context.Context, id string) (Webhook, error) {
	w, err := s.scanWebhook(s.db.QueryRowContext(ctx,
		`SELECT `+webhookCols+` FROM inbound_webhooks WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	return w, err
}

// GetByIDAndUser loads a webhook the user owns (management path).
func (s *Store) GetByIDAndUser(ctx context.Context, id, userID string) (Webhook, error) {
	w, err := s.scanWebhook(s.db.QueryRowContext(ctx,
		`SELECT `+webhookCols+` FROM inbound_webhooks WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	return w, err
}

// ListByUser returns the user's webhooks, newest first. The secret is part of
// the struct but callers must not serialize it.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+webhookCols+` FROM inbound_webhooks WHERE user_id = $1
		 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := s.scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Delete removes the user's webhook. It reports whether a row was removed.
func (s *Store) Delete(ctx context.Context, id, userID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inbound_webhooks WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RotateSecret replaces the webhook's secret, invalidating the old one.
func (s *Store) RotateSecret(ctx context.Context, id, userID, newSecret string) error {
	secret := newSecret
	if s.enc != nil {
		enc, err := s.enc.Encrypt(newSecret)
		if err != nil {
			return err
		}
		secret = enc
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE inbound_webhooks SET secret_cipher = $1, updated_at = now()
		 WHERE id = $2 AND user_id = $3`, secret, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEnabled flips the webhook on/off (owner management).
func (s *Store) SetEnabled(ctx context.Context, id, userID string, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inbound_webhooks SET enabled = $1, updated_at = now()
		 WHERE id = $2 AND user_id = $3`, enabled, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastUsed stamps the trigger's hit time (best-effort observability; a
// failed stamp must not fail the trigger).
func (s *Store) TouchLastUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE inbound_webhooks SET last_used_at = now() WHERE id = $1`, id)
	return err
}

// ErrReplay is returned when a trigger's nonce was already seen within the
// signature window — the same signed event was delivered twice.
var ErrReplay = errors.New("replayed nonce")

// ClaimNonce deduplicates a trigger's nonce: it records (webhook_id, nonce)
// and reports false — with ErrReplay — when the pair was already claimed. This
// is the trigger's hot path, so expired rows are NOT pruned here; the hourly
// SweepExpiredNonces pass keeps the table bounded to the signature window.
func (s *Store) ClaimNonce(ctx context.Context, webhookID, nonce string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO inbound_webhook_nonces (webhook_id, nonce, seen_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (webhook_id, nonce) DO NOTHING`,
		webhookID, nonce, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrReplay
	}
	return nil
}

// SweepExpiredNonces deletes nonce rows seen before cutoff and reports how
// many were removed. Called by the gateway's hourly sweep, off the trigger
// hot path; the grace (cutoff = now - signatureWindow - 1m) matches the
// per-claim prune the hot path used to run.
func (s *Store) SweepExpiredNonces(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inbound_webhook_nonces WHERE seen_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return n, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
