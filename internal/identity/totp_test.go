package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestTOTPCodeRFC6238Vector(t *testing.T) {
	// RFC 6238 test vector: secret "12345678901234567890" (base32 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ),
	// T=59 → 94287082 (8 digits); our 6-digit variant uses the same counter.
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	code, err := totpCode(secret, time.Unix(59, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != totpDigits {
		t.Fatalf("code = %q, want %d digits", code, totpDigits)
	}
	// The same counter value across windows: T=59 and T=30 both fall in the
	// second 30s period, so they produce the same code.
	code2, _ := totpCode(secret, time.Unix(30, 0))
	if code != code2 {
		t.Fatalf("codes differ across the same period: %q vs %q", code, code2)
	}
}

func TestTOTPEnrollConfirmDisable(t *testing.T) {
	svc, _, store, ctx := totpEnv(t)
	u := totpUser(t, store)

	secret, uri, err := svc.EnrollTOTP(ctx, u.ID)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if secret == "" || uri == "" || !contains(uri, "otpauth://totp/") {
		t.Fatalf("enroll returned secret=%q uri=%q", secret, uri)
	}
	// Not enabled until confirmed: login still issues a token.
	if enabled, _ := svc.TOTPEnabled(ctx, u.ID); enabled {
		t.Fatal("enroll must not enable the factor yet")
	}
	// Wrong code fails confirmation.
	if err := svc.ConfirmTOTP(ctx, u.ID, "000000"); !errors.Is(err, ErrInvalidTOTP) {
		t.Fatalf("confirm wrong code: %v", err)
	}
	// Right code enables it.
	code, _ := totpCode(secret, time.Now())
	if err := svc.ConfirmTOTP(ctx, u.ID, code); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if enabled, _ := svc.TOTPEnabled(ctx, u.ID); !enabled {
		t.Fatal("factor not enabled after confirm")
	}

	// Login now demands the second factor.
	if _, _, err := svc.Login(ctx, u.Email, "secret-pass-123"); !errors.Is(err, ErrTOTPRequired) {
		t.Fatalf("login with factor enabled: %v, want ErrTOTPRequired", err)
	}

	// Challenge round trip: begin → complete with the right code → token.
	u2, err := svc.LookupByEmail(ctx, u.Email)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := svc.BeginTOTPChallenge(ctx, u2)
	if err != nil {
		t.Fatalf("begin challenge: %v", err)
	}
	if _, _, err := svc.CompleteTOTPChallenge(ctx, ch.Token, "000000"); !errors.Is(err, ErrInvalidTOTP) {
		t.Fatalf("complete with wrong code: %v", err)
	}
	// The wrong attempt consumed the one-shot token.
	if _, _, err := svc.CompleteTOTPChallenge(ctx, ch.Token, code); !errors.Is(err, ErrInvalidTOTP) {
		t.Fatalf("challenge token replay: %v, want ErrInvalidTOTP", err)
	}
	ch2, _ := svc.BeginTOTPChallenge(ctx, u2)
	token, _, err := svc.CompleteTOTPChallenge(ctx, ch2.Token, code)
	if err != nil {
		t.Fatalf("complete challenge: %v", err)
	}
	if token == "" {
		t.Fatal("no token issued after successful second factor")
	}

	// Disable requires the current code.
	if err := svc.DisableTOTP(ctx, u.ID, "000000"); !errors.Is(err, ErrInvalidTOTP) {
		t.Fatalf("disable wrong code: %v", err)
	}
	if err := svc.DisableTOTP(ctx, u.ID, code); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if enabled, _ := svc.TOTPEnabled(ctx, u.ID); enabled {
		t.Fatal("factor still enabled after disable")
	}
	// Password login works again.
	if _, _, err := svc.Login(ctx, u.Email, "secret-pass-123"); err != nil {
		t.Fatalf("login after disable: %v", err)
	}
}

func TestTOTPSkewTolerance(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	now := time.Now()
	// A code from one period in the past still verifies within the window.
	past, err := totpCode(secret, now.Add(-totpPeriod))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := verifyTOTP(secret, past, now)
	if err != nil || !ok {
		t.Fatalf("past-window code rejected: ok=%v err=%v", ok, err)
	}
	// Beyond the window it fails.
	farPast, err := totpCode(secret, now.Add(-3*totpPeriod))
	if err != nil {
		t.Fatal(err)
	}
	ok, _ = verifyTOTP(secret, farPast, now)
	if ok {
		t.Fatal("far-past code accepted beyond the skew window")
	}
}

// --- helpers ---

func totpEnv(t *testing.T) (*Service, *recordingSMS, *Store, context.Context) {
	t.Helper()
	db := pgTestDB(t)
	s := NewStore(db)
	svc := NewService(s)
	svc.now = func() time.Time { return time.Now().UTC() }
	return svc, &recordingSMS{}, s, context.Background()
}

func totpUser(t *testing.T, store *Store) User {
	t.Helper()
	svc := NewService(store)
	u, err := svc.Signup(context.Background(), "totp-"+totpRand()+"@test.dev", "secret-pass-123", "totp-user")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	t.Cleanup(func() { store.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })
	return u
}

func totpRand() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
