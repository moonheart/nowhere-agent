package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		{"12345", ""},           // too short
		{"23800138000", ""},     // must start with 1
		{"138001380001", ""},    // too long
		{"13800138000a", ""},    // letters
		{"+8613800138000x", ""}, // junk
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

// TestCreatePhoneUserFirstAccountHonorsBootstrapFlag pins the phone path to
// the same bootstrap rule as email signup: the first account on an empty
// platform is admin only with the explicit opt-in (WithFirstAccountAdmin).
func TestCreatePhoneUserFirstAccountHonorsBootstrapFlag(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		store     *Store
		wantAdmin bool
	}{
		{"opt-in", NewStore(freshDB(t)).WithFirstAccountAdmin(true), true},
		{"default", NewStore(freshDB(t)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := tc.store.CreatePhoneUser(ctx, phoneNumber(), "First")
			if err != nil {
				t.Fatalf("create first phone user: %v", err)
			}
			if u.IsAdmin() != tc.wantAdmin {
				t.Errorf("first phone account platform_role = %q, want admin=%v", u.PlatformRole, tc.wantAdmin)
			}
		})
	}
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

func TestPhoneVerifyNonCodeErrorsDoNotCount(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE phone = $1`, phone)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})

	h := NewPhoneHandler(svc, sms)
	th := NewOTPThrottler()
	th.now = svc.now
	h.throttle = th

	// A wrong code IS a failed guess: it must count toward the lockout.
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatalf("request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/phone/verify",
		strings.NewReader(fmt.Sprintf(`{"phone":%q,"code":"000000"}`, phone)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.serveVerify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-code status = %d, want 401", rec.Code)
	}
	th.mu.Lock()
	if n := len(th.verify); n != 1 {
		t.Fatalf("wrong code must count once, got %d entries", n)
	}
	th.mu.Unlock()

	// Verifying the CORRECT code of a disabled user fails with ErrUserDisabled
	// — a server-side state error, not a failed guess: the throttle must not
	// count it (a DB hiccup or state error must not lock the pair).
	u, err := store.CreatePhoneUser(ctx, phone, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/phone/verify",
		strings.NewReader(fmt.Sprintf(`{"phone":%q,"code":%q}`, phone, sms.delivered[phone])))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.serveVerify(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled-user status = %d, want 403", rec.Code)
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if n := len(th.verify); n != 1 {
		t.Errorf("non-code errors must not count toward the verify throttle (map holds %d entries)", n)
	}
}

// bindPhoneTo requests a code for phone and binds it to the account, returning
// the delivered code (already consumed) — the setup half of reset/bind tests.
// The service clock is advanced past the resend cooldown so the next request
// in the test is allowed.
func bindPhoneTo(t *testing.T, svc *Service, sms *recordingSMS, userID, phone string) {
	t.Helper()
	if err := svc.RequestPhoneOTP(context.Background(), phone, sms); err != nil {
		t.Fatalf("request code: %v", err)
	}
	if err := svc.BindPhone(context.Background(), userID, phone, sms.delivered[phone]); err != nil {
		t.Fatalf("bind phone: %v", err)
	}
	advanceClock(svc, otpCooldown+time.Second)
}

func TestBindPhone(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	a, err := store.CreateUser(ctx, "a-"+phone+"@example.com", "correct-horse", "A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateUser(ctx, "b-"+phone+"@example.com", "correct-horse", "B")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, a.ID, b.ID)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})

	// Wrong code is refused without binding.
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindPhone(ctx, a.ID, phone, "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong code: %v, want ErrInvalidCode", err)
	}
	if _, err := store.UserByPhone(ctx, phone); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unexpected user by phone before bind: %v", err)
	}

	// Correct code binds; the account is resolvable by phone.
	advanceClock(svc, otpCooldown+time.Second)
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindPhone(ctx, a.ID, phone, sms.delivered[phone]); err != nil {
		t.Fatalf("bind: %v", err)
	}
	u, err := store.UserByPhone(ctx, phone)
	if err != nil {
		t.Fatalf("user by phone after bind: %v", err)
	}
	if u.ID != a.ID {
		t.Fatalf("phone bound to %s, want %s", u.ID, a.ID)
	}

	// Re-binding the SAME account is idempotent success.
	if err := svc.BindPhone(ctx, a.ID, phone, "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("idempotent rebind must still verify the code first: %v", err)
	}
	advanceClock(svc, otpCooldown+time.Second)
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindPhone(ctx, a.ID, phone, sms.delivered[phone]); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}

	// Another account cannot take a bound phone (the unique index holds).
	advanceClock(svc, otpCooldown+time.Second)
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindPhone(ctx, b.ID, phone, sms.delivered[phone]); !errors.Is(err, ErrPhoneTaken) {
		t.Fatalf("cross-account bind: %v, want ErrPhoneTaken", err)
	}

	// Invalid input shapes are refused up front.
	if err := svc.BindPhone(ctx, a.ID, "not-a-phone", "123456"); !errors.Is(err, ErrInvalidPhone) {
		t.Fatalf("invalid phone: %v", err)
	}
}

func TestResetPasswordByPhone(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	u, err := svc.Signup(ctx, "u-"+phone+"@example.com", "old-password-1", "U")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})

	bindPhoneTo(t, svc, sms, u.ID, phone)

	// A weak password is refused WITHOUT consuming the code.
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatal(err)
	}
	code := sms.delivered[phone]
	if err := svc.ResetPasswordByPhone(ctx, phone, code, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v, want ErrWeakPassword", err)
	}
	if _, _, err := svc.Login(ctx, u.Email, "old-password-1"); err != nil {
		t.Fatalf("login with old password after weak-password attempt: %v", err)
	}

	// The same code still resets the password.
	if err := svc.ResetPasswordByPhone(ctx, phone, code, "new-password-1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, _, err := svc.Login(ctx, u.Email, "old-password-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password still valid after reset: %v", err)
	}
	if _, _, err := svc.Login(ctx, u.Email, "new-password-1"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}

	// The code is single-use: replaying it cannot reset again.
	if err := svc.ResetPasswordByPhone(ctx, phone, code, "third-password-1"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("code replay: %v, want ErrInvalidCode", err)
	}

	// A wrong code never touches the password.
	advanceClock(svc, otpCooldown+time.Second)
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetPasswordByPhone(ctx, phone, "000000", "fourth-password-1"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong code: %v, want ErrInvalidCode", err)
	}
	if _, _, err := svc.Login(ctx, u.Email, "new-password-1"); err != nil {
		t.Fatalf("password changed by wrong code: %v", err)
	}

	// An unbound phone cannot reset (no account to target).
	unbound := phoneNumber()
	t.Cleanup(func() { store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, unbound) })
	advanceClock(svc, otpCooldown+time.Second)
	if err := svc.RequestPhoneOTP(ctx, unbound, sms); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetPasswordByPhone(ctx, unbound, sms.delivered[unbound], "any-password-1"); !errors.Is(err, ErrNoAccountForPhone) {
		t.Fatalf("unbound reset: %v, want ErrNoAccountForPhone", err)
	}
}

// TestPhoneResetHTTP wires the reset endpoint against the real handler: wrong
// codes 401 and count toward the verify throttle; a valid reset is 204 and the
// account can log in with the new password.
func TestPhoneResetHTTP(t *testing.T) {
	svc, sms, store, ctx := phoneEnv(t)
	phone := phoneNumber()
	u, err := store.CreateUser(ctx, "r-"+phone+"@example.com", "old-password-1", "R")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phone)
	})
	bindPhoneTo(t, svc, sms, u.ID, phone)

	h := NewPhoneHandler(svc, sms)
	th := NewOTPThrottler()
	th.now = svc.now
	h.throttle = th

	reset := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/phone/reset-password",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.serveResetPassword(rec, req)
		return rec
	}

	// Wrong code: 401 and one verify-throttle entry.
	if err := svc.RequestPhoneOTP(ctx, phone, sms); err != nil {
		t.Fatal(err)
	}
	rec := reset(fmt.Sprintf(`{"phone":%q,"code":"000000","password":"fresh-password-1"}`, phone))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-code status = %d, want 401", rec.Code)
	}
	th.mu.Lock()
	if n := len(th.verify); n != 1 {
		t.Fatalf("wrong code must count once, got %d entries", n)
	}
	th.mu.Unlock()

	// Weak password: 400, and the code survives for a valid attempt.
	rec = reset(fmt.Sprintf(`{"phone":%q,"code":%q,"password":"short"}`, phone, sms.delivered[phone]))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weak-password status = %d, want 400", rec.Code)
	}
	rec = reset(fmt.Sprintf(`{"phone":%q,"code":%q,"password":"fresh-password-1"}`, phone, sms.delivered[phone]))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204", rec.Code)
	}
	if _, _, err := svc.Login(ctx, u.Email, "fresh-password-1"); err != nil {
		t.Fatalf("login with reset password: %v", err)
	}

	// An unbound phone: 404, and it must NOT count toward the throttle (the
	// pair's history was cleared by the successful reset above, so it stays 0).
	unbound := phoneNumber()
	t.Cleanup(func() { store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, unbound) })
	if err := svc.RequestPhoneOTP(ctx, unbound, sms); err != nil {
		t.Fatal(err)
	}
	rec = reset(fmt.Sprintf(`{"phone":%q,"code":%q,"password":"fresh-password-1"}`, unbound, sms.delivered[unbound]))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unbound status = %d, want 404", rec.Code)
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if n := len(th.verify); n != 0 {
		t.Errorf("non-code errors must not count toward the verify throttle (map holds %d entries)", n)
	}
}
