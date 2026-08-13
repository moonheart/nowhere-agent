package identity

import (
	"context"
	"errors"
	"time"
)

// TOTP self-service + login challenge (MFA). Flow:
//
//	1. Enroll: POST /api/me/totp/enable → {secret, uri} (secret shown once).
//	2. Confirm: POST /api/me/totp/confirm {code} → validates and turns the
//	   flag on. Until confirmed the account's login is unchanged.
//	3. Login: a password login on an enabled account answers 200 with
//	   {totp_required: true, totp_token: <one-shot token>} instead of a bearer
//	   token; the caller then POSTs /api/auth/totp/verify {totp_token, code}
//	   and receives the platform token.
//	4. Disable: POST /api/me/totp/disable {code} — requires the current code.

// ErrTOTPRequired is returned by Login when the account has a second factor;
// the handler answers the challenge instead of failing.
var ErrTOTPRequired = errors.New("totp required")

// ErrInvalidTOTP means the submitted code is wrong.
var ErrInvalidTOTP = errors.New("invalid verification code")

// ErrTOTPNotEnabled means the account has no second factor to disable/verify.
var ErrTOTPNotEnabled = errors.New("totp not enabled")

// TOTPChallenge carries the login-time hand-off: a one-shot token the
// second-factor verify call redeems.
type TOTPChallenge struct {
	Token string
	User  User
}

// EnrollTOTP generates a fresh secret for the account (the current one, if
// any, is replaced). The secret and its otpauth URI are returned ONCE; the
// account is not protected until ConfirmTOTP succeeds.
func (s *Service) EnrollTOTP(ctx context.Context, userID string) (secret, uri string, err error) {
	secret, err = newTOTPSecret()
	if err != nil {
		return "", "", err
	}
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if err := s.store.SetTOTP(ctx, userID, secret, false); err != nil {
		return "", "", err
	}
	return secret, otpauthURI(secret, u.Email), nil
}

// ConfirmTOTP validates a code against the pending secret and enables the
// second factor. A wrong code leaves the account unchanged.
func (s *Service) ConfirmTOTP(ctx context.Context, userID, code string) error {
	secret, _, err := s.store.TOTPState(ctx, userID)
	if err != nil {
		return err
	}
	if secret == "" {
		return ErrTOTPNotEnabled
	}
	ok, err := verifyTOTP(secret, code, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidTOTP
	}
	return s.store.SetTOTP(ctx, userID, secret, true)
}

// DisableTOTP turns the second factor off; the current code must verify.
func (s *Service) DisableTOTP(ctx context.Context, userID, code string) error {
	secret, enabled, err := s.store.TOTPState(ctx, userID)
	if err != nil {
		return err
	}
	if !enabled || secret == "" {
		return ErrTOTPNotEnabled
	}
	ok, err := verifyTOTP(secret, code, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidTOTP
	}
	return s.store.SetTOTP(ctx, userID, "", false)
}

// BeginTOTPChallenge issues a one-shot token for the account's second-factor
// verify (the login hand-off). Tokens are single-use and short-lived.
func (s *Service) BeginTOTPChallenge(ctx context.Context, u User) (TOTPChallenge, error) {
	raw, err := generateToken()
	if err != nil {
		return TOTPChallenge{}, err
	}
	// Reuse the auth token machinery: a 5-minute one-shot token.
	exp := s.now().Add(5 * time.Minute)
	if err := s.store.CreateToken(ctx, u.ID, hashToken(raw), exp); err != nil {
		return TOTPChallenge{}, err
	}
	return TOTPChallenge{Token: raw, User: u}, nil
}

// CompleteTOTPChallenge redeems the one-shot token with the account's code
// and returns the platform bearer token. The challenge token is consumed
// either way (success or wrong code), so a code can never be replayed.
func (s *Service) CompleteTOTPChallenge(ctx context.Context, challengeToken, code string) (token string, u User, err error) {
	userID, err := s.store.UserIDByTokenHash(ctx, hashToken(challengeToken), s.now())
	if err != nil {
		return "", User{}, ErrInvalidTOTP
	}
	u, err = s.store.UserByID(ctx, userID)
	if err != nil {
		return "", User{}, ErrInvalidTOTP
	}
	// Consume the challenge token immediately (single-use).
	_ = s.store.DeleteToken(ctx, hashToken(challengeToken))

	secret, enabled, err := s.store.TOTPState(ctx, u.ID)
	if err != nil {
		return "", User{}, err
	}
	if !enabled || secret == "" {
		return "", User{}, ErrTOTPNotEnabled
	}
	ok, err := verifyTOTP(secret, code, s.now())
	if err != nil {
		return "", User{}, err
	}
	if !ok {
		// Carry the resolved user along with the error so the caller can
		// attribute the failure (throttling) without a second lookup.
		return "", u, ErrInvalidTOTP
	}
	if u.Disabled() {
		return "", User{}, ErrUserDisabled
	}
	raw, err := s.IssueToken(ctx, u)
	if err != nil {
		return "", User{}, err
	}
	return raw, u, nil
}
