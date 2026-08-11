package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// phoneEmailPrefix marks a phone-only account's synthetic email. The users
// email column is NOT NULL UNIQUE; a phone account gets
// "phone:<normalized-number>" (unique because the phone is unique), with an
// unusable password sentinel — the same provisioning shape OIDC accounts use.
// It can never be logged into with a password.
const phoneEmailPrefix = "phone:"

// phonePasswordSentinel marks an account that has no password (phone or OIDC
// provisioned): bcrypt could never produce it.
const phonePasswordSentinel = "!"

// UserByPhone fetches a user by normalized phone number.
func (s *Store) UserByPhone(ctx context.Context, phone string) (User, error) {
	return s.scanUser(ctx, `SELECT `+userColumns+` FROM users WHERE phone = $1`, phone)
}

// CreatePhoneUser provisions an account for a verified phone number. The
// first account on an empty platform becomes admin, exactly like email
// signup. Returns ErrUserExists when the phone already holds an account.
func (s *Store) CreatePhoneUser(ctx context.Context, phone, displayName string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin create phone user: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, bootstrapAdminLockKey); err != nil {
		return User{}, fmt.Errorf("bootstrap lock: %w", err)
	}

	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
		return User{}, fmt.Errorf("count users: %w", err)
	}
	role := PlatformRoleUser
	if existing == 0 {
		role = PlatformRoleAdmin
	}

	u, err := scanUserRow(tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, display_name, platform_role, phone)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+userColumns,
		phoneEmailPrefix+phone, phonePasswordSentinel, displayName, string(role), phone,
	))
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrUserExists
		}
		return User{}, fmt.Errorf("create phone user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit create phone user: %w", err)
	}
	return u, nil
}

// LatestOTP returns the newest unconsumed, unexpired code row for phone.
func (s *Store) LatestOTP(ctx context.Context, phone string, now time.Time) (OTP, error) {
	o, err := scanOTP(s.db.QueryRowContext(ctx, `
		SELECT id, phone, code_hash, expires_at, attempts, consumed_at, created_at
		FROM phone_otps
		WHERE phone = $1 AND consumed_at IS NULL AND expires_at > $2
		ORDER BY created_at DESC LIMIT 1`, phone, now))
	if errors.Is(err, sql.ErrNoRows) {
		return OTP{}, ErrNoOTP
	}
	return o, err
}

// CreateOTP records a pending code for phone.
func (s *Store) CreateOTP(ctx context.Context, phone, codeHash string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO phone_otps (phone, code_hash, expires_at)
		VALUES ($1, $2, $3)`, phone, codeHash, expiresAt)
	return err
}

// ConsumeOTP marks the code used (single-use) and reports whether a row was
// actually updated — the guard against double-spending a code.
func (s *Store) ConsumeOTP(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE phone_otps SET consumed_at = $1
		WHERE id = $2 AND consumed_at IS NULL`, now, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// BumpOTPAttempts counts one failed guess against a code. It returns the new
// attempt count.
func (s *Store) BumpOTPAttempts(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		UPDATE phone_otps SET attempts = attempts + 1
		WHERE id = $1 AND consumed_at IS NULL
		RETURNING attempts`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoOTP
	}
	return n, err
}

// RecentOTPCreatedAt returns the created_at of the newest OTP row for phone
// (used for resend-cooldown), or the zero time when none exists.
func (s *Store) RecentOTPCreatedAt(ctx context.Context, phone string) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT created_at FROM phone_otps WHERE phone = $1 ORDER BY created_at DESC LIMIT 1`, phone).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

// OTP is one pending phone-verification code.
type OTP struct {
	ID        string
	Phone     string
	CodeHash  string
	ExpiresAt time.Time
	Attempts  int
	Consumed  *time.Time
	CreatedAt time.Time
}

func scanOTP(row interface{ Scan(...any) error }) (OTP, error) {
	var o OTP
	err := row.Scan(&o.ID, &o.Phone, &o.CodeHash, &o.ExpiresAt, &o.Attempts, &o.Consumed, &o.CreatedAt)
	return o, err
}
