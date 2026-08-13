package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
)

// stubAccounts is an AccountProvisioner that records what it was asked to
// provision and returns a fixed account.
type stubAccounts struct {
	gotIssuer, gotSubject, gotEmail, gotName string
	user                                     identity.User
	err                                      error
}

func (s *stubAccounts) ProvisionExternalUser(_ context.Context, issuer, subject, email, name string) (identity.User, error) {
	s.gotIssuer, s.gotSubject, s.gotEmail, s.gotName = issuer, subject, email, name
	return s.user, s.err
}

// startLogin drives GET /auth/oidc/login and returns the redirect location plus
// the state cookie it set, so a test can then drive the callback as a browser
// would.
func startLogin(t *testing.T, h *Handler) (location string, stateCookie *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.login(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	res := rec.Result()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("login should redirect, got %d", res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("login must set a state cookie")
	}
	return res.Header.Get("Location"), stateCookie
}

func TestLoginRedirectsToIdPWithStateCookie(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	h := NewHandler(p, &stubAccounts{}, func(context.Context, identity.User) (string, error) { return "tok", nil })

	loc, cookie := startLogin(t, h)
	if !strings.Contains(loc, f.issuer()) {
		t.Fatalf("login should redirect to the IdP, got %q", loc)
	}
	if !strings.Contains(loc, "state="+cookie.Value) {
		t.Fatalf("redirect should carry the cookie's state, got %q vs %q", loc, cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Fatal("state cookie must be HttpOnly")
	}
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	h := NewHandler(p, &stubAccounts{}, func(context.Context, identity.User) (string, error) { return "tok", nil })

	// Drive login to mint a real state, then call back with a DIFFERENT state.
	_, cookie := startLogin(t, h)
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=forged&code=x", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	res := rec.Result()
	loc := res.Header.Get("Location")
	if !strings.Contains(loc, "#sso_error=") {
		t.Fatalf("state mismatch must redirect with an error, got %q", loc)
	}
	if strings.Contains(loc, "#token=") {
		t.Fatal("state mismatch must never issue a token")
	}
}

func TestCallbackRejectsMissingStateCookie(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	h := NewHandler(p, &stubAccounts{}, func(context.Context, identity.User) (string, error) { return "tok", nil })

	// No cookie at all (cross-site POST-less replay): refuse.
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=x&code=y", nil)
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	if loc := rec.Result().Header.Get("Location"); !strings.Contains(loc, "#sso_error=") {
		t.Fatalf("missing state cookie must redirect with an error, got %q", loc)
	}
}

func TestCallbackSurfacesIdPError(t *testing.T) {
	f := newFakeIdP(t)
	p := newProviderFor(t, f)
	h := NewHandler(p, &stubAccounts{}, func(context.Context, identity.User) (string, error) { return "tok", nil })

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?error=access_denied&error_description=user+cancelled", nil)
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	loc := rec.Result().Header.Get("Location")
	if !strings.Contains(loc, "#sso_error=") || !strings.Contains(loc, "access_denied") {
		t.Fatalf("IdP error should be surfaced, got %q", loc)
	}
}

func TestCallbackSuccessProvisionsAndIssuesToken(t *testing.T) {
	f := newFakeIdP(t)
	f.lastSub = "carol"
	p := newProviderFor(t, f)
	accounts := &stubAccounts{user: identity.User{ID: "u-1", Email: "carol@corp.test"}}
	h := NewHandler(p, accounts, func(_ context.Context, u identity.User) (string, error) {
		return "platform-token-" + u.ID, nil
	})

	_, cookie := startLogin(t, h)
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+cookie.Value+"&code=good", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	res := rec.Result()
	loc := res.Header.Get("Location")

	if !strings.Contains(loc, "#token=platform-token-u-1") {
		t.Fatalf("success should redirect with the platform token, got %q", loc)
	}
	// The account was provisioned from the VERIFIED claims (subject carol), not
	// from anything in the query string.
	if accounts.gotSubject != "carol" || accounts.gotIssuer != f.issuer() {
		t.Fatalf("provisioned from wrong claims: %+v", accounts)
	}
	// State cookie is single-use: cleared on the callback.
	for _, c := range res.Cookies() {
		if c.Name == stateCookieName && c.MaxAge >= 0 && c.Value != "" {
			t.Fatal("state cookie must be cleared after a successful callback")
		}
	}
}

func TestCallbackDisabledAccountGetsNoToken(t *testing.T) {
	f := newFakeIdP(t)
	f.lastSub = "dave"
	p := newProviderFor(t, f)
	now := time.Now()
	accounts := &stubAccounts{user: identity.User{ID: "u-2", Email: "dave@corp.test", DisabledAt: &now}}
	h := NewHandler(p, accounts, func(context.Context, identity.User) (string, error) { return "tok", nil })

	_, cookie := startLogin(t, h)
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+cookie.Value+"&code=good", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	loc := rec.Result().Header.Get("Location")
	if strings.Contains(loc, "#token=") {
		t.Fatal("a disabled account must never receive a token")
	}
	if !strings.Contains(loc, "disabled") {
		t.Fatalf("disabled account should see a disabled error, got %q", loc)
	}
}

// totpHandler wires a handler whose account always requires a second factor,
// so the callback lands in the TOTP deferral branch.
func totpHandler(t *testing.T, accounts *stubAccounts) *Handler {
	t.Helper()
	f := newFakeIdP(t)
	f.lastSub = "eve"
	p := newProviderFor(t, f)
	h := NewHandler(p, accounts, func(context.Context, identity.User) (string, error) { return "tok", nil })
	h.WithTotpChallenge(func(context.Context, identity.User) (string, error) { return "ch-1", nil })
	return h
}

func TestCallbackTotpUsesRelativeRedirectURI(t *testing.T) {
	accounts := &stubAccounts{user: identity.User{ID: "u-3", Email: "eve@corp.test"}}
	h := totpHandler(t, accounts)

	_, cookie := startLogin(t, h)
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+cookie.Value+"&code=good&redirect_uri=/settings", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	loc := rec.Result().Header.Get("Location")
	if !strings.Contains(loc, "/settings#totp_required=") {
		t.Fatalf("TOTP redirect should honor the relative path, got %q", loc)
	}
}

func TestCallbackTotpRejectsAbsoluteRedirectURI(t *testing.T) {
	accounts := &stubAccounts{user: identity.User{ID: "u-3", Email: "eve@corp.test"}}
	h := totpHandler(t, accounts)

	// An absolute URL (or a protocol-relative one) must be refused outright —
	// redirecting the authenticated browser there would be an open redirect.
	for _, bad := range []string{"https://evil.example/phish", "//evil.example/phish", "javascript:alert(1)"} {
		_, cookie := startLogin(t, h)
		req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+cookie.Value+"&code=good&redirect_uri="+url.QueryEscape(bad), nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.callback(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("redirect_uri=%q -> status %d, want 400", bad, rec.Code)
		}
		if loc := rec.Header().Get("Location"); strings.Contains(loc, "totp_required") {
			t.Errorf("redirect_uri=%q must never redirect to the challenge, got %q", bad, loc)
		}
	}
}
