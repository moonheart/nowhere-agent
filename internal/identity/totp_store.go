package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// totpColumns are the extra user fields for the second factor.
const totpColumns = `totp_secret, totp_enabled`

// SetTOTP stores (or clears) the enrolled secret and its enabled flag.
func (s *Store) SetTOTP(ctx context.Context, userID, secret string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET totp_secret = $1, totp_enabled = $2 WHERE id = $3`,
		nullIfEmptyStr(secret), enabled, userID)
	return err
}

// TOTPState loads the account's second-factor state.
func (s *Store) TOTPState(ctx context.Context, userID string) (secret string, enabled bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT `+totpColumns+` FROM users WHERE id = $1`, userID).Scan(&secret, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrUserNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("totp state: %w", err)
	}
	return secret, enabled, nil
}

func nullIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
