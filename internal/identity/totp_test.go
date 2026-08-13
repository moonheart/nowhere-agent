package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTOTPCodeRFC6238Vector(t *testing.T) {
	// RFC 6238 Appendix B test vectors (SHA-1, 8-digit reference values; our
	// 6-digit variant uses the same counter + dynamic truncation, so the
	// vectors below assert the EXACT codes, pinning the algorithm against
	// regression — a broken truncation would change every value).
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, tc := range []struct {
		unix int64
		code string
	}{
		// RFC 8-digit references truncated to 6: 94287082→287082, 07081804→081804,
		// 14050471→050471, 89005924→005924, 69279037→279037, 65353130→353130.
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		got, err := totpCode(secret, time.Unix(tc.unix, 0))
		if err != nil {
			t.Fatalf("T=%d: %v", tc.unix, err)
		}
		if got != tc.code {
			t.Errorf("T=%d: code = %q, want %q", tc.unix, got, tc.code)
		}
	}
	// Sanity: 6 digits exactly.
	code, _ := totpCode(secret, time.Unix(59, 0))
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

// TestTOTPVerifyThrottling locks a (user, ip) pair after repeated wrong codes:
// the verify endpoint refuses the pair and, once locked, login no longer mints
// challenge tokens for it. A correct code clears the counter.
func TestTOTPVerifyThrottling(t *testing.T) {
	svc, _, store, ctx := totpEnv(t)
	u := totpUser(t, store)
	secret, _, err := svc.EnrollTOTP(ctx, u.ID)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	code, _ := totpCode(secret, time.Now())
	if err := svc.ConfirmTOTP(ctx, u.ID, code); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	h := NewHandler(svc).WithTOTPThrottle(NewLoginThrottler())
	ip := "203.0.113.7"
	challenge := func(email string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/auth/login",
			strings.NewReader(fmt.Sprintf(`{"email":%q,"password":"secret-pass-123"}`, email)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":45678"
		w := httptest.NewRecorder()
		h.login(w, req)
		return w
	}

	for i := 0; i < 5; i++ {
		w := challenge(u.Email)
		if w.Code != http.StatusOK {
			t.Fatalf("challenge %d: status %d, want 200", i, w.Code)
		}
		var ch struct {
			Token string `json:"totp_token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil || ch.Token == "" {
			t.Fatalf("challenge %d: no token", i)
		}
		if w := verify(t, h, ip, fmt.Sprintf(`{"totp_token":%q,"code":"000000"}`, ch.Token)); w.Code != http.StatusUnauthorized {
			t.Fatalf("verify %d: status %d, want 401", i, w.Code)
		}
	}

	// The pair is now locked: login no longer mints challenge tokens for it.
	if w := challenge(u.Email); w.Code != http.StatusTooManyRequests {
		t.Fatalf("challenge after lock: status %d, want 429", w.Code)
	}

	// A fresh pair (different user) still works, then a success clears it.
	u2 := totpUser(t, store)
	secret2, _, err := svc.EnrollTOTP(ctx, u2.ID)
	if err != nil {
		t.Fatalf("enroll u2: %v", err)
	}
	code2, _ := totpCode(secret2, time.Now())
	if err := svc.ConfirmTOTP(ctx, u2.ID, code2); err != nil {
		t.Fatalf("confirm u2: %v", err)
	}
	w := challenge(u2.Email)
	if w.Code != http.StatusOK {
		t.Fatalf("challenge for untouched pair: status %d, want 200", w.Code)
	}
	var ch struct {
		Token string `json:"totp_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if w := verify(t, h, ip, fmt.Sprintf(`{"totp_token":%q,"code":%q}`, ch.Token, code2)); w.Code != http.StatusOK {
		t.Fatalf("verify with right code: status %d, want 200", w.Code)
	}
}

// verifyBody posts a totp verify request from ip and returns the recorder.
func verify(t *testing.T, h *Handler, ip, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/auth/totp/verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":45678"
	w := httptest.NewRecorder()
	h.totpVerify(w, req)
	return w
}

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
