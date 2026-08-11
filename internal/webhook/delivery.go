package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Delivery is one outbox row: a run-completion notification the platform has
// committed to delivering.
type Delivery struct {
	ID            string
	RunID         string
	SessionID     string
	UserID        string
	TargetURL     string
	Payload       json.RawMessage
	Status        string // pending | delivered | failed
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	DeliveredAt   *time.Time
}

// DeliveryStatus values.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// ErrNoPending is returned by ClaimNext when the queue holds nothing due.
var ErrNoPending = errors.New("no pending webhook delivery")

// DeliveryStore persists outbox rows in Postgres. Claims are atomic
// (UPDATE … WHERE status='pending' AND next_attempt_at <= now RETURNING), so
// multiple instances can run sweepers without double-sending.
type DeliveryStore struct {
	db *sql.DB
}

// NewDeliveryStore builds a DeliveryStore over a database handle.
func NewDeliveryStore(db *sql.DB) *DeliveryStore {
	return &DeliveryStore{db: db}
}

// Enqueue records a delivery and returns the row. userID links the row to
// the account it notifies about (ON DELETE CASCADE — deleting the account
// erases its outbox rows, PIPL §47); empty when unresolvable.
func (s *DeliveryStore) Enqueue(ctx context.Context, runID, sessionID, targetURL, userID string, payload any) (Delivery, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Delivery{}, err
	}
	return scanDelivery(s.db.QueryRowContext(ctx, `
		INSERT INTO webhook_deliveries (run_id, session_id, target_url, user_id, payload)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, run_id, session_id, user_id, target_url, payload, status, attempts,
		          next_attempt_at, last_error, created_at, delivered_at`,
		runID, sessionID, targetURL, nullIfEmpty(userID), raw))
}

// claimLease is how long a claimed delivery stays reserved: the claim moves
// next_attempt_at forward by the lease, so a slow in-flight attempt is not
// re-claimed by another instance (at-least-once with a bounded duplicate
// window instead of unbounded double-sends).
const claimLease = 5 * time.Minute

// ClaimNext atomically claims one due delivery (or ErrNoPending). The claim
// increments attempts and pushes next_attempt_at out by the lease, making the
// row invisible to other sweepers while it is in flight.
func (s *DeliveryStore) ClaimNext(ctx context.Context, now time.Time) (Delivery, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx, `
		UPDATE webhook_deliveries
		SET attempts = attempts + 1, next_attempt_at = $2
		WHERE id = (
			SELECT id FROM webhook_deliveries
			WHERE status = 'pending' AND next_attempt_at <= $1
			ORDER BY next_attempt_at LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, run_id, session_id, user_id, target_url, payload, status, attempts,
		          next_attempt_at, last_error, created_at, delivered_at`, now, now.Add(claimLease)))
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, ErrNoPending
	}
	return d, err
}

// MarkDelivered settles a claim.
func (s *DeliveryStore) MarkDelivered(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET status = 'delivered', delivered_at = $2, last_error = NULL
		WHERE id = $1`, id, now)
	return err
}

// MarkFailed dead-letters a claim (final) or schedules its next retry.
func (s *DeliveryStore) MarkFailed(ctx context.Context, id string, nextAttemptAt time.Time, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET last_error = $2, next_attempt_at = $3,
		    status = CASE WHEN $3 > now() THEN 'pending' ELSE 'failed' END
		WHERE id = $1`, id, errMsg, nextAttemptAt)
	return err
}

// List returns a page of deliveries, newest first, optionally filtered.
func (s *DeliveryStore) List(ctx context.Context, status string, limit, offset int) ([]Delivery, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, session_id, user_id, target_url, payload, status, attempts,
		       next_attempt_at, last_error, created_at, delivered_at
		FROM webhook_deliveries
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE ($1 = '' OR status = $1)`, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// Requeue resets a failed delivery to pending for a manual retry.
func (s *DeliveryStore) Requeue(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET status = 'pending', attempts = 0, last_error = NULL, next_attempt_at = now()
		WHERE id = $1 AND status = 'failed'`, id)
	return err
}

// Retention defaults: dead letters keep for 30 days (enough to diagnose and
// requeue), delivered rows for 90 (a bounded audit window — the payload
// carries a conversation summary, so unbounded retention would violate data
// minimization).
const (
	DeadLetterRetention = 30 * 24 * time.Hour
	DeliveredRetention  = 90 * 24 * time.Hour
)

// PurgeExpired removes rows past their retention window — dead letters after
// DeadLetterRetention, delivered rows after DeliveredRetention — keeping the
// outbox table bounded. It returns the number of rows removed.
func (s *DeliveryStore) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM webhook_deliveries
		WHERE (status = 'failed' AND created_at < $1)
		   OR (status = 'delivered' AND created_at < $2)`,
		now.Add(-DeadLetterRetention), now.Add(-DeliveredRetention))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanDelivery(row interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	var userID, lastError sql.NullString
	err := row.Scan(&d.ID, &d.RunID, &d.SessionID, &userID, &d.TargetURL, &d.Payload,
		&d.Status, &d.Attempts, &d.NextAttemptAt, &lastError, &d.CreatedAt, &d.DeliveredAt)
	d.UserID = userID.String
	d.LastError = lastError.String
	return d, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
