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

func (p *HTTPSMSProvider) Send(ctx context.Context, phone, code string) error {
	body, err := json.Marshal(map[string]string{"phone": phone, "code": code})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
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

// VerifyPhoneOTP checks the code (constant-time, attempt-capped, single-use)
// and, on success, provisions or resolves the account and issues the platform
// bearer token — the exact same token a password login returns.
func (s *Service) VerifyPhoneOTP(ctx context.Context, rawPhone, code string) (token string, u User, err error) {
	phone := NormalizePhone(rawPhone)
	if phone == "" {
		return "", User{}, ErrInvalidPhone
	}
	otp, err := s.store.LatestOTP(ctx, phone, s.now())
	if err != nil {
		return "", User{}, ErrInvalidCode
	}
	if otp.Attempts >= otpMaxAttempts {
		return "", User{}, ErrInvalidCode
	}
	if subtle.ConstantTimeCompare([]byte(hashCode(code)), []byte(otp.CodeHash)) != 1 {
		if n, berr := s.store.BumpOTPAttempts(ctx, otp.ID); berr == nil && n >= otpMaxAttempts {
			// Burn the code at the cap so the remaining window offers nothing.
			_, _ = s.store.ConsumeOTP(ctx, otp.ID, s.now())
		}
		return "", User{}, ErrInvalidCode
	}
	ok, err := s.store.ConsumeOTP(ctx, otp.ID, s.now())
	if err != nil {
		return "", User{}, err
	}
	if !ok {
		return "", User{}, ErrInvalidCode // already used (raced)
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
