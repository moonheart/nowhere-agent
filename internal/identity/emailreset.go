package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Email-based self-service password recovery — the recovery path for an
// account whose password was lost, for deployments without the phone channel.
// The flow mirrors ResetPasswordByPhone exactly (same OTP constants, same
// storage, same attempt cap and single-use semantics); only the identity key
// differs: the account's email instead of a bound phone.
//
// Delivery: the platform has NO mail channel today, so codes are handed to an
// EmailResetCodeProvider; the only built-in implementation prints the code to
// the server log (the dev/self-host path, mirroring the phone channel's
// "log://" mode). A deployment that grows SMTP wires its own provider.
//
// Storage: codes live in the shared phone_otps table keyed by the normalized
// email. The table's columns are generic over the key string, and phone keys
// (11 digits) and email keys (contain "@") can never collide, so no migration
// and no Store change is needed — the sweep, cooldown, and attempt-cap logic
// apply unchanged.

var (
	// ErrInvalidEmail means the submitted email is not a usable identifier
	// (empty or obviously malformed).
	ErrInvalidEmail = errors.New("invalid email")
	// ErrNoAccountForEmail means the email holds no account, so a password
	// reset cannot target anyone.
	ErrNoAccountForEmail = errors.New("no account for this email")
	// ErrNoPasswordForAccount means the account has no password to reset
	// (phone- or OIDC-provisioned accounts use an unusable sentinel).
	ErrNoPasswordForAccount = errors.New("account has no password to reset")
)

// EmailResetCodeProvider delivers a reset code to an account's email. The
// platform has no mail channel; the built-in provider logs the code, and a
// deployment with SMTP wires its own.
type EmailResetCodeProvider interface {
	Send(ctx context.Context, email, code string) error
}

// LogEmailResetCodeProvider prints the code to the server log — the
// dev/self-host path, mirroring the phone channel's log:// mode. It is NOT a
// delivery channel for production.
type LogEmailResetCodeProvider struct{ log *slog.Logger }

// NewLogEmailResetCodeProvider builds a log-backed provider.
func NewLogEmailResetCodeProvider(log *slog.Logger) *LogEmailResetCodeProvider {
	return &LogEmailResetCodeProvider{log: log}
}

func (p *LogEmailResetCodeProvider) Send(_ context.Context, email, code string) error {
	p.log.Warn("email password-reset code (log provider — dev only, NOT a delivery channel)",
		"email", email, "code", code)
	return nil
}

// validateEmailForReset rejects identifiers that could never address an
// account: empty, or lacking an "@" (no platform email can be shaped that
// way). Deliberately no stricter format check — the platform stores whatever
// signup accepted, and the lookup is exact.
func validateEmailForReset(email string) bool {
	return email != "" && strings.Contains(email, "@")
}

// RequestEmailResetCode validates the email, enforces the resend cooldown,
// mints a 6-digit code, and hands it to the provider. The code is stored
// hashed, keyed by the normalized email, in the shared phone_otps table. Like
// the phone channel, the request does NOT reveal whether an account holds the
// email — a 204 is returned either way, and only the reset step resolves the
// account — so this open route is not an account-enumeration oracle.
func (s *Service) RequestEmailResetCode(ctx context.Context, rawEmail string, provider EmailResetCodeProvider) error {
	email := normalizeEmail(rawEmail)
	if !validateEmailForReset(email) {
		return ErrInvalidEmail
	}
	last, err := s.store.RecentOTPCreatedAt(ctx, email)
	if err != nil {
		return err
	}
	if !last.IsZero() && s.now().Sub(last) < otpCooldown {
		return ErrOTPTooSoon
	}

	code, err := newOTPCode()
	if err != nil {
		return err
	}
	if provider != nil {
		if err := provider.Send(ctx, email, code); err != nil {
			return err
		}
	}
	return s.store.CreateOTP(ctx, email, hashCode(code), s.now().Add(otpTTL))
}

// ResetPasswordByEmail resets an account's password after OTP verification
// proves possession of a reset code addressed to the account's email — the
// self-service recovery path for a lost password. It shares the admin
// ResetPassword semantics (SetPassword revokes every session) and deliberately
// bypasses TOTP: a user who lost the password may have lost the authenticator
// too, and the OTP is the recovery credential. Accounts without a password
// (phone- or OIDC-provisioned, sentinel hash) are refused: there is nothing
// to recover.
func (s *Service) ResetPasswordByEmail(ctx context.Context, rawEmail, code, newPassword string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	email := normalizeEmail(rawEmail)
	if !validateEmailForReset(email) {
		return ErrInvalidEmail
	}
	if err := s.consumeOTP(ctx, email, code); err != nil {
		return err
	}
	u, err := s.store.UserByEmail(ctx, email)
	if errors.Is(err, ErrUserNotFound) {
		return ErrNoAccountForEmail
	}
	if err != nil {
		return err
	}
	if u.Disabled() {
		return ErrUserDisabled
	}
	// Sentinel hashes ("!..." — bcrypt always starts "$2") mark accounts with
	// no password: nothing to reset, and minting one would hand a phone/SSO
	// account a second credential out of a code the mailbox never received.
	if strings.HasPrefix(u.PasswordHash, "!") {
		return ErrNoPasswordForAccount
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return s.store.SetPassword(ctx, u.ID, string(hash))
}
