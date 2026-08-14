package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordingEmail captures delivered codes so tests can submit them.
type recordingEmail struct {
	delivered map[string]string // email -> code
}

func (r *recordingEmail) Send(_ context.Context, email, code string) error {
	if r.delivered == nil {
		r.delivered = map[string]string{}
	}
	r.delivered[email] = code
	return nil
}

// emailEnv wires a Service + recording provider over the dev DB.
func emailEnv(t *testing.T) (*Service, *recordingEmail, *Store, context.Context) {
	t.Helper()
	db := pgTestDB(t)
	s := NewStore(db)
	svc := NewService(s)
	svc.now = func() time.Time { return time.Now().UTC() }
	return svc, &recordingEmail{}, s, context.Background()
}

// resetEmail returns a unique address for one test run.
func resetEmail() string {
	return "reset-" + randSuffix() + "@test.dev"
}

func TestEmailResetRoundTrip(t *testing.T) {
	svc, mail, store, ctx := emailEnv(t)
	email := resetEmail()
	u, err := svc.Signup(ctx, email, "old-password-1", "U")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, email)
	})

	if err := svc.RequestEmailResetCode(ctx, email, mail); err != nil {
		t.Fatalf("request: %v", err)
	}
	code, ok := mail.delivered[email]
	if !ok || len(code) != 6 {
		t.Fatalf("no 6-digit code delivered: %q", code)
	}
	// The stored row (in the shared phone_otps table, keyed by email) must
	// hold the HASH, not the code.
	otp, err := store.LatestOTP(ctx, email, svc.now())
	if err != nil {
		t.Fatalf("latest otp: %v", err)
	}
	if otp.CodeHash == code || otp.CodeHash == "" {
		t.Fatalf("code stored in plaintext: %q", otp.CodeHash)
	}

	// A weak password is refused WITHOUT consuming the code.
	if err := svc.ResetPasswordByEmail(ctx, email, code, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v, want ErrWeakPassword", err)
	}
	if _, _, err := svc.Login(ctx, email, "old-password-1"); err != nil {
		t.Fatalf("login with old password after weak-password attempt: %v", err)
	}

	// The same code still resets the password.
	if err := svc.ResetPasswordByEmail(ctx, email, code, "new-password-1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, _, err := svc.Login(ctx, email, "old-password-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password still valid after reset: %v", err)
	}
	if _, _, err := svc.Login(ctx, email, "new-password-1"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}

	// The code is single-use: replaying it cannot reset again.
	if err := svc.ResetPasswordByEmail(ctx, email, code, "third-password-1"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("code replay: %v, want ErrInvalidCode", err)
	}

	// A wrong code never touches the password.
	advanceClock(svc, otpCooldown+time.Second)
	if err := svc.RequestEmailResetCode(ctx, email, mail); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetPasswordByEmail(ctx, email, "000000", "fourth-password-1"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong code: %v, want ErrInvalidCode", err)
	}
	if _, _, err := svc.Login(ctx, email, "new-password-1"); err != nil {
		t.Fatalf("password changed by wrong code: %v", err)
	}
}

func TestEmailResetCooldown(t *testing.T) {
	svc, mail, store, ctx := emailEnv(t)
	email := resetEmail()
	t.Cleanup(func() { store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, email) })

	if err := svc.RequestEmailResetCode(ctx, email, mail); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := svc.RequestEmailResetCode(ctx, email, mail); !errors.Is(err, ErrOTPTooSoon) {
		t.Fatalf("immediate second request: %v, want ErrOTPTooSoon", err)
	}
}

func TestEmailResetNoAccount(t *testing.T) {
	svc, mail, store, ctx := emailEnv(t)
	email := resetEmail()
	t.Cleanup(func() { store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, email) })

	// The request succeeds even for an email nobody holds (no enumeration
	// oracle on the open route)...
	if err := svc.RequestEmailResetCode(ctx, email, mail); err != nil {
		t.Fatalf("request: %v", err)
	}
	// ...but the reset itself must refuse to target anyone.
	if err := svc.ResetPasswordByEmail(ctx, email, mail.delivered[email], "any-password-1"); !errors.Is(err, ErrNoAccountForEmail) {
		t.Fatalf("no-account reset: %v, want ErrNoAccountForEmail", err)
	}

	// An invalid email is refused up front.
	if err := svc.RequestEmailResetCode(ctx, "not-an-email", mail); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("invalid email request: %v, want ErrInvalidEmail", err)
	}
	if err := svc.ResetPasswordByEmail(ctx, "not-an-email", "123456", "any-password-1"); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("invalid email reset: %v, want ErrInvalidEmail", err)
	}
}

func TestEmailResetPasswordlessAccountRefused(t *testing.T) {
	svc, mail, store, ctx := emailEnv(t)
	// An SSO-provisioned account holds a real email but an unusable sentinel
	// password: there is no password to recover, and minting one would hand
	// the account a second credential the (log-only) mailbox never received.
	email := "sso-" + randSuffix() + "@test.dev"
	u, err := store.ProvisionExternalUser(ctx, "https://idp.test.dev", "sub-"+randSuffix(), email, "S")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, email)
	})
	if err := svc.RequestEmailResetCode(ctx, email, mail); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := svc.ResetPasswordByEmail(ctx, email, mail.delivered[email], "fresh-password-1"); !errors.Is(err, ErrNoPasswordForAccount) {
		t.Fatalf("passwordless reset: %v, want ErrNoPasswordForAccount", err)
	}
}

func TestEmailResetAttemptCapBurnsCode(t *testing.T) {
	svc, mail, store, ctx := emailEnv(t)
	email := resetEmail()
	u, err := svc.Signup(ctx, email, "old-password-1", "U")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, email)
	})

	if err := svc.RequestEmailResetCode(ctx, email, mail); err != nil {
		t.Fatalf("request: %v", err)
	}
	code := mail.delivered[email]
	for i := 0; i < otpMaxAttempts; i++ {
		if err := svc.ResetPasswordByEmail(ctx, email, "000000", "any-password-1"); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// After the cap the CORRECT code is also refused (the code was burned).
	if err := svc.ResetPasswordByEmail(ctx, email, code, "any-password-1"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("correct code after cap: %v, want ErrInvalidCode", err)
	}
}

// TestEmailResetHTTP wires the reset endpoints against the real handler:
// wrong codes 401 and count toward the (shared) verify throttle; a valid
// reset is 204 and the account can log in with the new password.
func TestEmailResetHTTP(t *testing.T) {
	svc, mail, store, ctx := emailEnv(t)
	email := resetEmail()
	u, err := svc.Signup(ctx, email, "old-password-1", "U")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
		store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, email)
	})

	h := NewEmailResetHandler(svc, mail)
	th := NewOTPThrottler()
	th.now = svc.now
	h.throttle = th

	reqCode := func(email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/email/reset-code",
			strings.NewReader(fmt.Sprintf(`{"email":%q}`, email)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.serveRequestCode(rec, req)
		return rec
	}
	reset := func(email, code, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/email/reset-password",
			strings.NewReader(fmt.Sprintf(`{"email":%q,"code":%q,"password":%q}`, email, code, password)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.serveResetPassword(rec, req)
		return rec
	}

	// Request-code: 204 for a known email.
	if rec := reqCode(email); rec.Code != http.StatusNoContent {
		t.Fatalf("request-code status = %d, want 204", rec.Code)
	}

	// Wrong code: 401 and one verify-throttle entry.
	rec := reset(email, "000000", "fresh-password-1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-code status = %d, want 401", rec.Code)
	}
	th.mu.Lock()
	if n := len(th.verify); n != 1 {
		t.Fatalf("wrong code must count once, got %d entries", n)
	}
	th.mu.Unlock()

	// Weak password: 400, and the code survives for a valid attempt.
	rec = reset(email, mail.delivered[email], "short")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weak-password status = %d, want 400", rec.Code)
	}
	rec = reset(email, mail.delivered[email], "fresh-password-1")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204", rec.Code)
	}
	if _, _, err := svc.Login(ctx, email, "fresh-password-1"); err != nil {
		t.Fatalf("login with reset password: %v", err)
	}

	// Unknown email: 404, and it must NOT count toward the throttle (the
	// pair's history was cleared by the successful reset above, so it stays 0).
	unknown := "nobody-" + randSuffix() + "@test.dev"
	t.Cleanup(func() { store.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, unknown) })
	advanceClock(svc, otpCooldown+time.Second)
	if rec := reqCode(unknown); rec.Code != http.StatusNoContent {
		t.Fatalf("request-code for unknown email status = %d, want 204", rec.Code)
	}
	rec = reset(unknown, mail.delivered[unknown], "fresh-password-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-email status = %d, want 404", rec.Code)
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if n := len(th.verify); n != 0 {
		t.Errorf("non-code errors must not count toward the verify throttle (map holds %d entries)", n)
	}
}
