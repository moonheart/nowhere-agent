package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Phone/OTP authentication (domestic enterprise account convention): sign in
// with a mobile number + one-time code. The flow is deliberately symmetric
// with email login downstream — verification provisions/resolves the platform
// account and issues the platform's own bearer token, so RequireAuth/teams/
// quotas are unchanged.

const (
	// otpTTL is how long a code stays valid.
	otpTTL = 10 * time.Minute
	// otpCooldown is the minimum wait between sends to one phone.
	otpCooldown = 60 * time.Second
	// otpMaxAttempts caps wrong guesses per code before it is burned.
	otpMaxAttempts = 5
)

var (
	// ErrNoOTP means no pending code exists for the phone.
	ErrNoOTP = errors.New("no pending verification code")
	// ErrOTPTooSoon means a code was sent too recently; wait and retry.
	ErrOTPTooSoon = errors.New("verification code sent too recently")
	// ErrInvalidCode means the submitted code does not match, or the code was
	// burned by too many attempts.
	ErrInvalidCode = errors.New("invalid verification code")
	// ErrInvalidPhone means the number is not a valid Chinese mobile.
	ErrInvalidPhone = errors.New("invalid phone number")
	// ErrNoAccountForPhone means an OTP-verified phone has no bound account, so
	// a password reset cannot target anyone (a phone-only account would have
	// been created at verify time).
	ErrNoAccountForPhone = errors.New("no account bound to this phone")
	// ErrPhoneTaken means the phone is already bound to another account.
	ErrPhoneTaken = errors.New("phone is bound to another account")
)

// cnMobile matches a mainland Chinese mobile number: 11 digits starting with
// 1, followed by any digit (the carrier prefix space is wide enough that a
// stricter second-digit check would reject valid new ranges).
var cnMobile = regexp.MustCompile(`^1\d{10}$`)

// NormalizePhone strips spaces, dashes, parentheses, and an optional +86
// country prefix, returning the canonical 11-digit form, or "" when the input
// is not a valid Chinese mobile. Any other non-digit character rejects the
// input (a letter in a phone number is a typo, not something to silently
// drop).
func NormalizePhone(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '+' || r == '(' || r == ')':
			// separators and country prefix: strip
		default:
			return ""
		}
	}
	cleaned := b.String()
	if strings.HasPrefix(cleaned, "86") && len(cleaned) == 13 {
		cleaned = cleaned[2:]
	}
	if !cnMobile.MatchString(cleaned) {
		return ""
	}
	return cleaned
}

// SMSProvider delivers a verification code to a phone. The deployment wires
// its own gateway (阿里云/腾讯云 SMS, an internal HTTP adapter, …); the
// platform only hands over the code.
type SMSProvider interface {
	Send(ctx context.Context, phone, code string) error
}

// LogSMSProvider prints the code to the server log — the dev/self-host path
// (SMS_URL "log://"). It is NOT a delivery channel for production.
type LogSMSProvider struct{ log *slog.Logger }

// NewLogSMSProvider builds a log-backed provider.
func NewLogSMSProvider(log *slog.Logger) *LogSMSProvider {
	return &LogSMSProvider{log: log}
}

func (p *LogSMSProvider) Send(_ context.Context, phone, code string) error {
	p.log.Warn("phone OTP (log provider — dev only, NOT a delivery channel)",
		"phone", maskPhone(phone), "code", code)
	return nil
}

// HTTPSMSProvider POSTs {"phone","code"} to a deployment-owned URL, which
// performs the actual SMS send (an internal gateway adapter). 2xx = sent.
type HTTPSMSProvider struct {
	url    string
	client *http.Client
	log    *slog.Logger
}

// NewHTTPSMSProvider builds an HTTP-backed provider. timeout bounds one call.
func NewHTTPSMSProvider(url string, timeout time.Duration, log *slog.Logger) *HTTPSMSProvider {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPSMSProvider{url: url, client: &http.Client{Timeout: timeout}, log: log}
}

// RuntimeSMSProvider resolves the delivery channel from the runtime settings
// on EVERY send, so the admin console can switch gateways, switch to the log
// provider, or disable phone login without a restart: "log://" prints the
// code to the server log, an http(s) URL POSTs to the gateway, and "" fails
// closed (phone login disabled). A malformed URL fails the send with a clear
// error rather than guessing.
type RuntimeSMSProvider struct {
	urlFor     func() string
	timeoutFor func() time.Duration
	log        *slog.Logger
}

// NewRuntimeSMSProvider builds the runtime-resolved provider. urlFor must
// return "" (disabled), "log://", or an http(s) URL; timeoutFor bounds one
// gateway call (<= 0 falls back to 10s).
func NewRuntimeSMSProvider(urlFor func() string, timeoutFor func() time.Duration, log *slog.Logger) *RuntimeSMSProvider {
	return &RuntimeSMSProvider{urlFor: urlFor, timeoutFor: timeoutFor, log: log}
}

// Send resolves the current channel and delivers (or fails closed).
func (p *RuntimeSMSProvider) Send(ctx context.Context, phone, code string) error {
	url := strings.TrimSpace(p.urlFor())
	switch {
	case url == "":
		return errors.New("phone login disabled: no SMS gateway configured")
	case url == "log://":
		p.log.Warn("phone OTP (log provider — dev only, NOT a delivery channel)",
			"phone", maskPhone(phone), "code", code)
		return nil
	case !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://"):
		return fmt.Errorf("sms gateway: unsupported URL %q (want http(s):// or log://)", url)
	}
	timeout := p.timeoutFor()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	body, err := json.Marshal(map[string]string{"phone": phone, "code": code})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sms gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sms gateway: status %d", resp.StatusCode)
	}
	p.log.Info("phone OTP delivered via gateway", "phone", maskPhone(phone))
	return nil
}

// maskPhone keeps the last four digits, so logs never carry the full number.
func maskPhone(phone string) string {
	if len(phone) <= 4 {
		return "****"
	}
	return "****" + phone[len(phone)-4:]
}

// RequestPhoneOTP validates the number, enforces the resend cooldown, mints a
// 6-digit code, and hands it to the provider. The code is stored hashed; the
// provider failure aborts before any row is written, so a broken gateway
// cannot burn a code the user never received.
func (s *Service) RequestPhoneOTP(ctx context.Context, rawPhone string, provider SMSProvider) error {
	phone := NormalizePhone(rawPhone)
	if phone == "" {
		return ErrInvalidPhone
	}
	last, err := s.store.RecentOTPCreatedAt(ctx, phone)
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
		if err := provider.Send(ctx, phone, code); err != nil {
			return err
		}
	}
	return s.store.CreateOTP(ctx, phone, hashCode(code), s.now().Add(otpTTL))
}

// consumeOTP validates the pending code for phone (constant-time,
// attempt-capped) and single-use-consumes it on success. It is the shared
// credential gate of every phone-verified operation: login/verify, password
// reset, and phone binding.
func (s *Service) consumeOTP(ctx context.Context, phone, code string) error {
	otp, err := s.store.LatestOTP(ctx, phone, s.now())
	if err != nil {
		return ErrInvalidCode
	}
	if otp.Attempts >= otpMaxAttempts {
		return ErrInvalidCode
	}
	if subtle.ConstantTimeCompare([]byte(hashCode(code)), []byte(otp.CodeHash)) != 1 {
		if n, berr := s.store.BumpOTPAttempts(ctx, otp.ID); berr == nil && n >= otpMaxAttempts {
			// Burn the code at the cap so the remaining window offers nothing.
			_, _ = s.store.ConsumeOTP(ctx, otp.ID, s.now())
		}
		return ErrInvalidCode
	}
	ok, err := s.store.ConsumeOTP(ctx, otp.ID, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCode // already used (raced)
	}
	return nil
}

// VerifyPhoneOTP checks the code (constant-time, attempt-capped, single-use)
// and, on success, provisions or resolves the account and issues the platform
// bearer token — the exact same token a password login returns.
func (s *Service) VerifyPhoneOTP(ctx context.Context, rawPhone, code string) (token string, u User, err error) {
	phone := NormalizePhone(rawPhone)
	if phone == "" {
		return "", User{}, ErrInvalidPhone
	}
	if err := s.consumeOTP(ctx, phone, code); err != nil {
		return "", User{}, err
	}
	u, err = s.store.UserByPhone(ctx, phone)
	if errors.Is(err, ErrUserNotFound) {
		u, err = s.store.CreatePhoneUser(ctx, phone, "用户 "+phone[len(phone)-4:])
	}
	if err != nil {
		return "", User{}, err
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

// ResetPasswordByPhone resets an account's password after OTP verification
// proves possession of the phone bound to it — the self-service recovery path
// for an account whose password was lost. It shares the admin ResetPassword
// semantics (SetPassword revokes every session), and deliberately bypasses
// TOTP: a user who lost the password may have lost the authenticator too, and
// the OTP is the recovery credential. Only a phone actually bound to an
// account can reset it (verify would have created a phone-only account for a
// fresh number, so no account = no reset).
func (s *Service) ResetPasswordByPhone(ctx context.Context, rawPhone, code, newPassword string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	phone := NormalizePhone(rawPhone)
	if phone == "" {
		return ErrInvalidPhone
	}
	if err := s.consumeOTP(ctx, phone, code); err != nil {
		return err
	}
	u, err := s.store.UserByPhone(ctx, phone)
	if errors.Is(err, ErrUserNotFound) {
		return ErrNoAccountForPhone
	}
	if err != nil {
		return err
	}
	if u.Disabled() {
		return ErrUserDisabled
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return s.store.SetPassword(ctx, u.ID, string(hash))
}

// BindPhone verifies possession of the phone via OTP, then binds it to the
// caller's account (replacing any previous phone of theirs). The unique index
// on users.phone rejects a number another account holds. Binding is the
// prerequisite for phone-based password recovery, so it is reserved for the
// authenticated account owner (POST /api/me/phone/bind).
func (s *Service) BindPhone(ctx context.Context, userID, rawPhone, code string) error {
	phone := NormalizePhone(rawPhone)
	if phone == "" {
		return ErrInvalidPhone
	}
	if err := s.consumeOTP(ctx, phone, code); err != nil {
		return err
	}
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Phone == phone {
		return nil // already bound to this account: idempotent success
	}
	return s.store.SetUserPhone(ctx, userID, phone)
}

// newOTPCode returns a cryptographically random 6-digit code.
func newOTPCode() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand otp: %w", err)
	}
	n := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
	return fmt.Sprintf("%06d", n%1_000_000), nil
}

// hashCode hashes a code for storage (SHA-256 hex), so a DB read leaks no
// usable code.
func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
