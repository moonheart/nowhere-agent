package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/reqctx"
)

// UserFromContext returns the authenticated user, or false. The value lives in
// reqctx, the shared typed home for request-scoped values; this wrapper keeps
// the concrete identity.User type here.
func UserFromContext(ctx context.Context) (User, bool) {
	v, ok := reqctx.User(ctx)
	u, _ := v.(User)
	return u, ok
}

// NewContextWithUser stores u on the context, as RequireAuth does. Exported so
// other packages' tests can build authenticated requests without a database.
func NewContextWithUser(ctx context.Context, u User) context.Context {
	return reqctx.WithUser(ctx, u)
}

// Handler exposes identity endpoints over HTTP.
type Handler struct {
	svc *Service
	// audit records authentication events (signup/login/logout); nil disables
	// recording while the handler keeps serving normally.
	audit *audit.Logger
	// throttle, when set, locks a (email, ip) pair after repeated login
	// failures (credential-stuffing defense).
	throttle *LoginThrottler
	// totpThrottle, when set, locks a (userID, ip) pair after repeated wrong
	// second-factor codes — a TOTP account's password is already known to the
	// caller, so the 6-digit code is the only secret left to protect. The same
	// pair gates how many challenge tokens a login may mint.
	totpThrottle *LoginThrottler
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// WithThrottle wires login failure throttling. Left nil, /api/auth/login has
// no per-pair lockout (the gateway's global request limiter still applies).
func (h *Handler) WithThrottle(t *LoginThrottler) *Handler {
	h.throttle = t
	return h
}

// WithTOTPThrottle wires second-factor throttling. Left nil, /api/auth/totp/
// verify has no per-pair lockout.
func (h *Handler) WithTOTPThrottle(t *LoginThrottler) *Handler {
	h.totpThrottle = t
	return h
}

// WithAudit wires the audit trail so authentication events are recorded.
// Recording is best-effort: a write failure is logged, never surfaced to the
// client, so the trail can never become a login's single point of failure.
func (h *Handler) WithAudit(l *audit.Logger) *Handler {
	h.audit = l
	return h
}

// Register mounts identity routes on the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/signup", h.signup)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
	mux.HandleFunc("POST /api/auth/totp/verify", h.totpVerify)
	mux.HandleFunc("GET /api/me", h.requireAuth(h.me))
}

// RequireAuth is middleware that resolves the bearer token to a user and
// stores it on the request context (see UserFromContext). It rejects requests
// without a valid token with 401. Exported so other endpoints (e.g. chat) can
// protect their routes.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(h.requireAuth(next.ServeHTTP))
}

type credentialsRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type authResponse struct {
	Token string  `json:"token"`
	User  userDTO `json:"user"`
}

type userDTO struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	// PlatformRole tells the client whether to offer platform administration.
	// It is presentation only: every platform route enforces the role
	// server-side regardless of what the client renders.
	PlatformRole string `json:"platform_role"`
	Disabled     bool   `json:"disabled"`
}

func toDTO(u User) userDTO {
	role := u.PlatformRole
	if role == "" {
		role = PlatformRoleUser
	}
	return userDTO{
		ID:           u.ID,
		Email:        u.Email,
		DisplayName:  u.DisplayName,
		PlatformRole: string(role),
		Disabled:     u.Disabled(),
	}
}

// teamMembershipDTO is one of the caller's teams with their role in it.
type teamMembershipDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

func toTeamMembershipDTOs(teams []TeamWithRole) []teamMembershipDTO {
	out := make([]teamMembershipDTO, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamMembershipDTO{ID: t.Team.ID, Name: t.Team.Name, Role: string(t.Role)})
	}
	return out
}

// maxAuthBodyBytes bounds an auth request body at 1 MiB. Credential payloads
// are tiny (email, password, codes); anything larger gets 413 before decoding.
const maxAuthBodyBytes = 1 << 20

// readAuthBody decodes a JSON auth body into v via the shared httpx
// implementation: 413 for an oversized body (never decoded as truncated JSON),
// 400 for unreadable or malformed input.
func readAuthBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return httpx.DecodeBody(w, r, maxAuthBodyBytes, v)
}

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !readAuthBody(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password required")
		return
	}
	u, err := h.svc.Signup(r.Context(), req.Email, req.Password, req.DisplayName)
	if errors.Is(err, ErrUserExists) {
		h.record(audit.Failure(audit.ActionAuthSignup).FromRequest(r).Detail(map[string]any{"reason": "email_taken"}))
		writeError(w, http.StatusConflict, "user already exists")
		return
	}
	if errors.Is(err, ErrWeakPassword) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		h.record(audit.Failure(audit.ActionAuthSignup).FromRequest(r).Detail(map[string]any{"reason": "internal"}))
		writeError(w, http.StatusInternalServerError, "signup failed")
		return
	}
	h.record(audit.Success(audit.ActionAuthSignup).FromRequest(r).Actor(u.ID, u.Email).Target("user", u.ID))
	writeJSON(w, http.StatusCreated, map[string]any{"user": toDTO(u)})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !readAuthBody(w, r, &req) {
		return
	}
	// Login throttling gate: a locked (email, ip) pair is refused before any
	// credential work, with Retry-After so the client knows when to retry.
	// A nil throttler keeps the endpoint unthrottled.
	if h.throttle != nil {
		if allowed, retryAfter := h.throttle.Check(req.Email, audit.ClientIP(r)); !allowed {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
			return
		}
	}
	token, u, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if errors.Is(err, ErrInvalidCredentials) {
		// Record the attempted email but no actor id: a failed login has no
		// authenticated identity, and logging the address (not the password) is
		// what a credential-stuffing review needs.
		if h.throttle != nil {
			h.throttle.Fail(req.Email, audit.ClientIP(r))
		}
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"email": req.Email, "reason": "invalid_credentials"}))
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if errors.Is(err, ErrUserDisabled) {
		if h.throttle != nil {
			h.throttle.Fail(req.Email, audit.ClientIP(r))
		}
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).Detail(map[string]any{"reason": "account_disabled"}))
		writeError(w, http.StatusForbidden, "account is disabled")
		return
	}
	if errors.Is(err, ErrTOTPRequired) {
		// The password was right; the account demands its second factor. Issue
		// a one-shot challenge token and let the caller complete the login —
		// this is a 200 with a challenge, not an error: the credential half
		// succeeded and the flow continues.
		u, _ = h.svc.LookupByEmail(r.Context(), req.Email)
		// Minting is gated on the same (user, ip) pair as verify: a locked
		// pair must not be able to refresh its one-shot token to retry, and a
		// lockout must not be drownable in fresh challenges.
		if h.totpThrottle != nil {
			if allowed, retryAfter := h.totpThrottle.Check(u.ID, audit.ClientIP(r)); !allowed {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
				writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
				return
			}
		}
		challenge, cerr := h.svc.BeginTOTPChallenge(r.Context(), u)
		if cerr != nil {
			h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"email": req.Email, "reason": "totp_challenge_failed"}))
			writeError(w, http.StatusInternalServerError, "login failed")
			return
		}
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).
			Detail(map[string]any{"method": "password+totp", "reason": "awaiting_second_factor"}))
		writeJSON(w, http.StatusOK, map[string]any{
			"totp_required": true,
			"totp_token":    challenge.Token,
			"user":          toDTO(u),
		})
		return
	}
	if err != nil {
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"email": req.Email, "reason": "internal"}))
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	if h.throttle != nil {
		h.throttle.Success(req.Email, audit.ClientIP(r))
	}
	h.record(audit.Success(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).Target("user", u.ID))
	writeJSON(w, http.StatusOK, authResponse{Token: token, User: toDTO(u)})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token != "" {
		_ = h.svc.Logout(r.Context(), token)
		if u, ok := UserFromContext(r.Context()); ok {
			h.record(audit.Success(audit.ActionAuthLogout).FromRequest(r).Actor(u.ID, u.Email))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// record writes one event to the audit trail when one is wired. It is a no-op
// otherwise, and never affects the response — LogAndReport swallows the error.
func (h *Handler) record(e audit.Event) {
	if h.audit != nil {
		h.audit.LogAndReport(context.Background(), e)
	}
}

// totpVerify completes a password+TOTP login: it redeems the one-shot
// challenge token from /api/auth/login with the authenticator code and
// returns the platform bearer token.
func (h *Handler) totpVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TotpToken string `json:"totp_token"`
		Code      string `json:"code"`
	}
	if !readAuthBody(w, r, &req) {
		return
	}
	token, u, err := h.svc.CompleteTOTPChallenge(r.Context(), req.TotpToken, req.Code)
	if err != nil {
		if errors.Is(err, ErrInvalidTOTP) {
			// Count wrong codes per (user, ip); a challenge token can only be
			// obtained with the right password, so failures here are a focused
			// brute-force on the 6-digit secret. Failures with no resolvable
			// user (forged/expired token) are not counted: a random 256-bit
			// token cannot be guessed and minting is gated at login.
			if h.totpThrottle != nil && u.ID != "" {
				h.totpThrottle.Fail(u.ID, audit.ClientIP(r))
			}
			h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"method": "totp", "reason": "invalid_code"}))
			writeError(w, http.StatusUnauthorized, "invalid verification code")
			return
		}
		if errors.Is(err, ErrUserDisabled) {
			h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"method": "totp", "reason": "account_disabled"}))
			writeError(w, http.StatusForbidden, "account is disabled")
			return
		}
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"method": "totp", "reason": "internal"}))
		writeError(w, http.StatusInternalServerError, "verification failed")
		return
	}
	if h.totpThrottle != nil {
		h.totpThrottle.Success(u.ID, audit.ClientIP(r))
	}
	h.record(audit.Success(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).
		Target("user", u.ID).Detail(map[string]any{"method": "password+totp"}))
	writeJSON(w, http.StatusOK, authResponse{Token: token, User: toDTO(u)})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	// Teams ride along on the profile so the console can render its team
	// navigation from one request rather than a second round trip on load.
	teams, err := h.svc.TeamsForUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load teams failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":  toDTO(u),
		"teams": toTeamMembershipDTOs(teams),
	})
}

// requireAuth is middleware that resolves the bearer token to a user.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			// No query-param fallback (removed): a ?token= in the URL would
			// leak into reverse-proxy logs, browser history and Referer headers.
			// Image reads use the authenticated fetch helper (Bearer header),
			// so no route needs the query form.
			writeError(w, http.StatusUnauthorized, "missing token")
			return
		}
		u, err := h.svc.Authenticate(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r.WithContext(NewContextWithUser(r.Context(), u)))
	}
}

// bearerToken extracts the token from the Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.JSON(w, status, v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.Error(w, status, msg)
}
