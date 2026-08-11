package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// recordingSMS captures delivered codes so tests can submit them.
type recordingSMS struct {
	delivered map[string]string // phone -> code
}

func (r *recordingSMS) Send(_ context.Context, phone, code string) error {
	if r.delivered == nil {
		r.delivered = map[string]string{}
	}
	r.delivered[phone] = code
	return nil
}

func TestNormalizePhone(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"13800138000", "13800138000"},
		{"138 0013 8000", "13800138000"},
		{"+86 13800138000", "13800138000"},
		{"8613800138000", "13800138000"},
		{"(138)0013-8000", "13800138000"},
		{"", ""},
		{"12345", ""},            // too short
		{"23800138000", ""},      // must start with 1
		{"138001380001", ""},     // too long
		{"13800138000a", ""},     // letters
		{"+8613800138000x", ""},  // junk
	} {
		if got := NormalizePhone(tc.in); got != tc.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// phoneEnv wires a Service + recording provider over the dev DB. The service
// clock is exposed so tests can advance past the resend cooldown.
func phoneEnv(t *testing.T) (*Service, *recordingSMS, *Store, context.Context) {
	t.Helper()
	db := pgTestDB(t)
	s := NewStore(db)
	svc := NewService(s)
	svc.now = func() time.Time { return time.Now().UTC() }
	return svc, &recordingSMS{}, s, context.Background()
}

// advanceClock fast-forwards the service clock by d (for cooldown tests).
func advanceClock(svc *Service, d time.Duration) {
	base := svc.now()
	svc.now = func() time.Time { return base.Add(d) }
}

func phoneNumber() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	digits := hex.EncodeToString(b)
	digits = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'f' {
			return rune('0' + int(r-'a'))
		}
		return r
	}, digits)
	return "138" + digits
}

func TestPhoneOTPRoundTrip(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE phone = $1`, phone)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})

	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatalf("request: %v", err)
	}
	code, ok := sms.delivered[phone]
	if !ok || len(code) != 6 {
		t.Fatalf("no 6-digit code delivered: %q", code)
	}
	// The stored row must hold the HASH, not the code.
	otp, err := store.LatestOTP(ctx, phone, svc.now())
	if err != nil {
		t.Fatalf("latest otp: %v", err)
	}
	if otp.CodeHash == code || otp.CodeHash == "" {
		t.Fatalf("code stored in plaintext: %q", otp.CodeHash)
	}

	// Wrong code is refused and burns an attempt.
	if _, _, err := svc.VerifyPhoneOTP(ctx, phone, "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong code: %v", err)
	}
	// Correct code logs in AND provisions the account.
	token, u, err := svc.VerifyPhoneOTP(ctx, phone, code)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if token == "" || u.Phone != phone {
		t.Fatalf("verify returned token=%q user.phone=%q", token, u.Phone)
	}
	if u.Email != phoneEmailPrefix+phone {
		t.Fatalf("phone account email = %q, want sentinel", u.Email)
	}
	// The code is single-use: replaying it fails.
	if _, _, err := svc.VerifyPhoneOTP(ctx, phone, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("code replay: %v, want ErrInvalidCode", err)
	}
	// Second login via a fresh code resolves the SAME account.
	advanceClock(svc, otpCooldown+time.Second)
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatalf("request 2: %v", err)
	}
	_, u2, err := svc.VerifyPhoneOTP(ctx, phone, sms.delivered[phone])
	if err != nil {
		t.Fatalf("verify 2: %v", err)
	}
	if u2.ID != u.ID {
		t.Fatalf("second login provisioned a NEW account: %s vs %s", u2.ID, u.ID)
	}
}

func TestPhoneOTPCooldown(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})

	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := svc.RequestPhoneOTP(ctx, phone, sms); !errors.Is(err, ErrOTPTooSoon) {
		t.Fatalf("immediate second request: %v, want ErrOTPTooSoon", err)
	}
}

func TestPhoneOTPAttemptCapBurnsCode(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE phone = $1`, phone)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})

	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatalf("request: %v", err)
	}
	code := sms.delivered[phone]
	for i := 0; i < otpMaxAttempts; i++ {
		if _, _, err := svc.VerifyPhoneOTP(ctx, phone, "000000"); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// After the cap the CORRECT code is also refused (the code was burned).
	if _, _, err := svc.VerifyPhoneOTP(ctx, phone, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("correct code after cap: %v, want ErrInvalidCode", err)
	}
}

func TestPhoneOTPInvalidPhone(t *testing.T) {
	svc, sms, _, ctx := phoneEnv(t)
	if err := svc.RequestPhoneOTP(ctx, "not-a-phone", sms); !errors.Is(err, ErrInvalidPhone) {
		t.Fatalf("request invalid phone: %v", err)
	}
	if _, _, err := svc.VerifyPhoneOTP(ctx, "not-a-phone", "123456"); !errors.Is(err, ErrInvalidPhone) {
		t.Fatalf("verify invalid phone: %v", err)
	}
}

func TestPhoneOTPExpiredCode(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})

	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatalf("request: %v", err)
	}
	// Age the code past its TTL.
	if _, err := store.db.Exec(
		`UPDATE phone_otps SET expires_at = $1 WHERE phone = $2`,
		svc.now().Add(-time.Minute), phone); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.VerifyPhoneOTP(ctx, phone, sms.delivered[phone]); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired code: %v, want ErrInvalidCode", err)
	}
}
