package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// totpColumns are the extra user fields for the second factor.
const totpColumns = `totp_secret, totp_enabled`

// SetTOTP stores (or clears) the enrolled secret and its enabled flag. The
// secret is encrypted at rest when the store carries an encryptor; a
// tampered-looking ciphertext on read is an error there, never a silent
// plaintext fallback (see secrets.Decrypt).
func (s *Store) SetTOTP(ctx context.Context, userID, secret string, enabled bool) error {
	if s.enc != nil && secret != "" {
		enc, err := s.enc.Encrypt(secret)
		if err != nil {
			return fmt.Errorf("totp secret encrypt: %w", err)
		}
		secret = enc
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET totp_secret = $1, totp_enabled = $2 WHERE id = $3`,
		nullIfEmptyStr(secret), enabled, userID)
	return err
}

// TOTPState loads the account's second-factor state, decrypting the seed on
// the way out. A legacy plaintext row (written before encryption was enabled)
// reads through unchanged and is re-encrypted on its next write. A cleared
// factor is a NULL seed, which scans as ("", false) — never a scan error.
func (s *Store) TOTPState(ctx context.Context, userID string) (secret string, enabled bool, err error) {
	var ns sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT `+totpColumns+` FROM users WHERE id = $1`, userID).Scan(&ns, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrUserNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("totp state: %w", err)
	}
	secret = ns.String
	if s.enc != nil && secret != "" {
		plain, derr := s.enc.Decrypt(secret)
		if derr != nil {
			return "", false, fmt.Errorf("totp secret decrypt: %w", derr)
		}
		secret = plain
	}
	return secret, enabled, nil
}

func nullIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
