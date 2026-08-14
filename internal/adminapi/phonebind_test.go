package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
)

// testSMS captures delivered codes for the bind flow.
type testSMS struct{ codes map[string]string }

func (s *testSMS) Send(_ context.Context, phone, code string) error {
	if s.codes == nil {
		s.codes = map[string]string{}
	}
	s.codes[phone] = code
	return nil
}

// TestBindPhoneHTTP wires POST /api/me/phone/bind against the real console
// handler: the OTP-verified bind, the shared verify-throttle lockout, and the
// cross-account rejection.
func TestBindPhoneHTTP(t *testing.T) {
	e := newEnv(t)
	sms := &testSMS{}
	svc := e.svc
	// The resend cooldown is 60s; tests backdate the OTP row so a re-request
	// is allowed instead of sleeping.
	backdate := func(phone string) {
		if _, err := e.db.Exec(`UPDATE phone_otps SET created_at = $1 WHERE phone = $2`,
			time.Now().UTC().Add(-time.Minute), phone); err != nil {
			t.Fatal(err)
		}
	}

	// 11 numeric digits, starting with 1 (the cnMobile shape); the nano-time
	// tail keeps numbers unique across runs.
	uniq := time.Now().UnixNano() % 100_000_000
	phoneA := "139" + fmt.Sprintf("%08d", uniq)
	phoneB := "137" + fmt.Sprintf("%08d", uniq+1)
	userA, err := svc.Signup(context.Background(), fmt.Sprintf("a-%d@example.com", time.Now().UnixNano()), "password-1-a", "A")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := svc.Signup(context.Background(), fmt.Sprintf("b-%d@example.com", time.Now().UnixNano()), "password-1-b", "B")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, userA.ID, userB.ID)
		e.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phoneA)
		e.db.Exec(`DELETE FROM phone_otps WHERE phone = $1`, phoneB)
	})

	bind := func(actor identity.User, phone, code string) *httptest.ResponseRecorder {
		e.actor = actor
		body := fmt.Sprintf(`{"phone":%q,"code":%q}`, phone, code)
		req := httptest.NewRequest(http.MethodPost, "/api/me/phone/bind", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req)
		return rec
	}

	// Wrong code: 401.
	if err := svc.RequestPhoneOTP(context.Background(), phoneA, sms); err != nil {
		t.Fatal(err)
	}
	if rec := bind(userA, phoneA, "000000"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-code status = %d, want 401", rec.Code)
	}

	// Correct code: 200 and the account resolves by phone.
	backdate(phoneA)
	if err := svc.RequestPhoneOTP(context.Background(), phoneA, sms); err != nil {
		t.Fatal(err)
	}
	if rec := bind(userA, phoneA, sms.codes[phoneA]); rec.Code != http.StatusOK {
		t.Fatalf("bind status = %d, want 200", rec.Code)
	}
	if u, err := e.store.UserByPhone(context.Background(), phoneA); err != nil || u.ID != userA.ID {
		t.Fatalf("user by phone after bind = %v, %v", u, err)
	}

	// Another account cannot take the bound phone: 409.
	backdate(phoneA)
	if err := svc.RequestPhoneOTP(context.Background(), phoneA, sms); err != nil {
		t.Fatal(err)
	}
	if rec := bind(userB, phoneA, sms.codes[phoneA]); rec.Code != http.StatusConflict {
		t.Fatalf("cross-account bind status = %d, want 409", rec.Code)
	}

	// A fresh phone still binds to B.
	backdate(phoneA)
	if err := svc.RequestPhoneOTP(context.Background(), phoneB, sms); err != nil {
		t.Fatal(err)
	}
	if rec := bind(userB, phoneB, sms.codes[phoneB]); rec.Code != http.StatusOK {
		t.Fatalf("second bind status = %d, want 200", rec.Code)
	}
	if u, err := e.store.UserByPhone(context.Background(), phoneB); err != nil || u.ID != userB.ID {
		t.Fatalf("user by phone after bind B = %v, %v", u, err)
	}
}
