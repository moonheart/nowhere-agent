package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// serviceKeyColumns is the projection every service-key query shares, in the
// order scanServiceKey expects.
const serviceKeyColumns = `id, name, user_id, created_at, expires_at, last_used_at, revoked_at`

// CreateServiceKey stores a service key (token already hashed by the caller).
// expiresAt nil means the key never expires.
func (s *Store) CreateServiceKey(ctx context.Context, name, userID, tokenHash string, expiresAt *time.Time) (ServiceKey, error) {
	key, err := scanServiceKey(s.db.QueryRowContext(ctx, `
		INSERT INTO service_keys (name, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+serviceKeyColumns,
		name, userID, tokenHash, expiresAt))
	if err != nil {
		return ServiceKey{}, fmt.Errorf("create service key: %w", err)
	}
	return key, nil
}

// UserIDByServiceKeyHash resolves a valid (not revoked, not expired) service
// key to its user id, touching last_used_at so operators can see active keys.
// Returns ErrInvalidToken for an unknown/revoked/expired key.
func (s *Store) UserIDByServiceKeyHash(ctx context.Context, tokenHash string, now time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin resolve service key: %w", err)
	}
	defer tx.Rollback()

	var userID string
	err = tx.QueryRowContext(ctx, `
		SELECT user_id FROM service_keys
		WHERE token_hash = $1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > $2)`,
		tokenHash, now).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("resolve service key: %w", err)
	}
	// last_used_at is observability, never load-bearing: a failed touch must
	// not fail the request it happened to measure.
	if _, err := tx.ExecContext(ctx, `UPDATE service_keys SET last_used_at = $1 WHERE token_hash = $2`, now, tokenHash); err != nil {
		return "", fmt.Errorf("touch service key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit resolve service key: %w", err)
	}
	return userID, nil
}

// ListServiceKeys returns every non-revoked key (admin listing), or only one
// user's keys when userID is non-empty. Revoked keys are included for audit
// visibility when includeRevoked is set.
func (s *Store) ListServiceKeys(ctx context.Context, userID string, includeRevoked bool) ([]ServiceKey, error) {
	q := `SELECT ` + serviceKeyColumns + ` FROM service_keys`
	var args []any
	conds := []string{}
	if userID != "" {
		conds = append(conds, `user_id = $1`)
		args = append(args, userID)
	}
	if !includeRevoked {
		conds = append(conds, `revoked_at IS NULL`)
	}
	if len(conds) > 0 {
		q += ` WHERE ` + conds[0]
		for _, c := range conds[1:] {
			q += ` AND ` + c
		}
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list service keys: %w", err)
	}
	defer rows.Close()
	out := []ServiceKey{}
	for rows.Next() {
		k, err := scanServiceKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeServiceKey soft-deletes a key (revoked_at set; the row stays for
// audit). Returns ErrKeyNotFound when the id matches nothing.
func (s *Store) RevokeServiceKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE service_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		if IsMalformedID(err) {
			return ErrKeyNotFound
		}
		return fmt.Errorf("revoke service key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// scanServiceKey decodes the serviceKeyColumns projection.
func scanServiceKey(row rowScanner) (ServiceKey, error) {
	var (
		k             ServiceKey
		expires, last sql.NullTime
		revoked       sql.NullTime
	)
	err := row.Scan(&k.ID, &k.Name, &k.UserID, &k.CreatedAt, &expires, &last, &revoked)
	if err != nil {
		return ServiceKey{}, err
	}
	if expires.Valid {
		t := expires.Time
		k.ExpiresAt = &t
	}
	if last.Valid {
		t := last.Time
		k.LastUsedAt = &t
	}
	if revoked.Valid {
		t := revoked.Time
		k.RevokedAt = &t
	}
	return k, nil
}
