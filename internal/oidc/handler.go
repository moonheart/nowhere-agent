package oidc

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/identity"
)

// stateCookieName carries the CSRF state from the login redirect to the
// callback. It is a short-lived, HttpOnly, SameSite=Lax cookie: HttpOnly keeps
// it out of JS, Lax lets the IdP's top-level redirect back carry it while
// blocking cross-site POSTs.
const stateCookieName = "nowhere_oidc_state"

// AccountProvisioner resolves or creates the platform account for a verified
// external identity and reports whether it may sign in. *identity.Store's
// ProvisionExternalUser satisfies the resolution half; the disabled check is
// applied here against the returned account.
type AccountProvisioner interface {
	ProvisionExternalUser(ctx context.Context, issuer, subject, email, displayName string) (identity.User, error)
}

// TokenIssuer issues the platform's own bearer token for an authenticated
// account — the same token a password login returns. *identity.Service's
// issueToken is wrapped by the server.
type TokenIssuer func(ctx context.Context, u identity.User) (string, error)

// TotpChallenger begins a second-factor challenge for an account that has one
// enabled. It returns the one-shot challenge token, or "" when the account
// has no second factor (the callback then proceeds to issue the token
// directly). The server implements it over identity's TOTP flow, so SSO
// logins respect the same MFA policy password logins do — a TOTP-enabled
// account cannot bypass its second factor via the IdP.
type TotpChallenger func(ctx context.Context, u identity.User) (string, error)

// Handler serves the browser-facing SSO endpoints: GET /auth/oidc/login
// (redirect to the IdP) and GET /auth/oidc/callback (code exchange + account
// provisioning + platform-token hand-off).
type Handler struct {
	provider   *Provider
	accounts   AccountProvisioner
	issueToken TokenIssuer
	// totpChallenge, when set, defers accounts with a second factor to the
	// MFA challenge step instead of issuing a token.
	totpChallenge TotpChallenger
	audit         *audit.Logger
	now           func() time.Time
}

// NewHandler wires an OIDC handler. issueToken turns a provisioned account into
// the platform bearer token the SPA then uses exactly as after a password login.
func NewHandler(p *Provider, accounts AccountProvisioner, issueToken TokenIssuer) *Handler {
	return &Handler{provider: p, accounts: accounts, issueToken: issueToken, now: time.Now}
}

// WithTotpChallenge wires the second-factor deferral (MFA parity with the
// password path). Nil skips the check (legacy behavior).
func (h *Handler) WithTotpChallenge(c TotpChallenger) *Handler {
	h.totpChallenge = c
	return h
}

// WithAudit wires the audit trail so SSO sign-ins are recorded (best-effort,
// like password logins — a recording failure never blocks sign-in).
func (h *Handler) WithAudit(l *audit.Logger) *Handler {
	h.audit = l
	return h
}

// Register mounts the SSO routes. They are intentionally NOT under RequireAuth:
// the whole point is to authenticate a browser that has no platform token yet.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/oidc/login", h.login)
	mux.HandleFunc("GET /auth/oidc/callback", h.callback)
}

// EnabledProbe returns an http.HandlerFunc answering {"enabled":true} for the
// SSO availability probe the SPA login page calls to decide whether to render
// the "Sign in with SSO" button. It is registered only when SSO is configured;
// a 404 (route absent) reads as "SSO off" to the client, so no disabled-branch
// handler is needed. The probe reveals nothing sensitive — just that SSO exists.
func EnabledProbe() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}
}

// login starts the flow: mint a state, stash it in a cookie, and redirect the
// browser to the IdP's authorization endpoint.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	state, err := NewState()
	if err != nil {
		http.Error(w, "sso unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/auth/oidc",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
		Expires:  h.now().Add(10 * time.Minute),
	})
	http.Redirect(w, r, h.provider.AuthURL(state), http.StatusFound)
}

// callback completes the flow. It validates the echoed state against the cookie,
// exchanges the code, provisions/resolves the account, and delivers the platform
// token to the SPA by redirecting to /#token=... (a fragment, so the token never
// reaches a server log or the browser history of a shared URL). Any failure
// redirects to /#sso_error=... so the login page can render it.
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if oidcErr := q.Get("error"); oidcErr != "" {
		// The IdP itself refused (user cancelled, access_denied, ...). Surface its
		// description, which is meant for the user, not a leaked secret.
		desc := q.Get("error_description")
		h.redirectError(w, r, oidcErr+": "+desc)
		return
	}

	cookie, err := r.Cookie(stateCookieName)
	if err != nil || cookie.Value == "" || cookie.Value != q.Get("state") {
		// State mismatch: this callback did not follow a login we started — a CSRF
		// or login-confusion attempt, or a stale/replayed link. Refuse.
		h.redirectError(w, r, "invalid or expired sso state")
		return
	}
	// Single-use: clear the state cookie so it cannot be replayed.
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: "", Path: "/auth/oidc", MaxAge: -1})

	code := q.Get("code")
	if code == "" {
		h.redirectError(w, r, "missing authorization code")
		return
	}

	claims, err := h.provider.Exchange(r.Context(), code)
	if err != nil {
		h.redirectError(w, r, "sso exchange failed")
		return
	}

	u, err := h.accounts.ProvisionExternalUser(r.Context(), claims.Issuer, claims.Subject, claims.Email, claims.Name)
	if err != nil {
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"method": "oidc", "reason": "provision"}))
		h.redirectError(w, r, "sso provisioning failed")
		return
	}
	if u.Disabled() {
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).Detail(map[string]any{"method": "oidc", "reason": "account_disabled"}))
		h.redirectError(w, r, "account is disabled")
		return
	}

	// MFA parity: an account with a second factor is deferred to the TOTP
	// challenge step (fragment #totp_required=<challenge>) instead of getting
	// a platform token — the IdP cannot bypass the platform's own policy.
	if h.totpChallenge != nil {
		if ch, cerr := h.totpChallenge(r.Context(), u); cerr != nil {
			h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).Detail(map[string]any{"method": "oidc", "reason": "totp_challenge"}))
			h.redirectError(w, r, "sso second-factor setup failed")
			return
		} else if ch != "" {
			h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).
				Detail(map[string]any{"method": "oidc+totp", "reason": "awaiting_second_factor"}))
			redirect := r.URL.Query().Get("redirect_uri")
			if redirect == "" {
				redirect = "/"
			}
			http.Redirect(w, r, redirect+"#totp_required="+url.QueryEscape(ch), http.StatusFound)
			return
		}
	}

	token, err := h.issueToken(r.Context(), u)
	if err != nil {
		h.redirectError(w, r, "sso token issue failed")
		return
	}

	h.record(audit.Success(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).Target("user", u.ID).Detail(map[string]any{"method": "oidc"}))

	// Hand the token to the SPA via a URL fragment: fragments are not sent to the
	// server on the follow-up load and do not appear in access logs. The login
	// page reads #token=..., stores it, and strips it from the address bar.
	http.Redirect(w, r, "/#token="+token, http.StatusFound)
}

// redirectError sends the browser back to the SPA login page with an error in
// the fragment (never a query param, so it is not logged server-side).
func (h *Handler) redirectError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/#sso_error="+url.QueryEscape(msg), http.StatusFound)
}

// record writes one event to the audit trail when one is wired (no-op otherwise).
func (h *Handler) record(e audit.Event) {
	if h.audit != nil {
		h.audit.LogAndReport(context.Background(), e)
	}
}
