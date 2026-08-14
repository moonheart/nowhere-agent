package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"nowhere-agent/internal/audit"
)

// EmailResetHandler serves the self-service email password-recovery routes:
// POST /api/auth/email/reset-code and POST /api/auth/email/reset-password.
// Both are OPEN routes (no bearer token — the caller has none yet) and are
// always registered: unlike the phone channel there is no delivery gateway to
// disable, codes go to the server log (or a deployment-wired provider).
type EmailResetHandler struct {
	svc      *Service
	provider EmailResetCodeProvider
	// throttle, when set, is the shared OTP throttler — the same instance the
	// phone routes use, so an (email, ip) pair is locked everywhere. Keys are
	// distinct strings (emails contain "@", phones are 11 digits), so the two
	// channels never interfere.
	throttle *OTPThrottler
	audit    *audit.Logger
	log      *slog.Logger
}

// NewEmailResetHandler builds the email recovery handler over the service and
// the delivery provider.
func NewEmailResetHandler(svc *Service, provider EmailResetCodeProvider) *EmailResetHandler {
	return &EmailResetHandler{
		svc: svc, provider: provider, throttle: NewOTPThrottler(), log: slog.Default(),
	}
}

// WithThrottle shares the phone channel's OTP throttler.
func (h *EmailResetHandler) WithThrottle(t *OTPThrottler) *EmailResetHandler {
	h.throttle = t
	return h
}

// WithAudit wires the audit trail (best-effort).
func (h *EmailResetHandler) WithAudit(l *audit.Logger) *EmailResetHandler {
	h.audit = l
	return h
}

// SetLogger overrides the handler's logger.
func (h *EmailResetHandler) SetLogger(l *slog.Logger) *EmailResetHandler {
	if l != nil {
		h.log = l
	}
	return h
}

// Register mounts the open routes.
func (h *EmailResetHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/email/reset-code", h.serveRequestCode)
	mux.HandleFunc("POST /api/auth/email/reset-password", h.serveResetPassword)
}

type emailResetCodeRequest struct {
	Email string `json:"email"`
}

type emailResetRequest struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

// serveRequestCode mints a one-time code for the email. The response is 204
// whether or not an account holds the email — this open route must not double
// as an account-enumeration oracle; only the reset step resolves the account.
func (h *EmailResetHandler) serveRequestCode(w http.ResponseWriter, r *http.Request) {
	var req emailResetCodeRequest
	if !readAuthBody(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	if !validateEmailForReset(email) {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}
	ip := audit.ClientIP(r)
	// Anti-code-flooding: daily quotas per email and per IP (the per-code 60s
	// cooldown in the service is the fine-grained wall underneath).
	if err := h.throttle.AllowSend(email, ip); err != nil {
		h.record(audit.Failure(audit.ActionEmailResetCodeRequest).FromRequest(r).
			Detail(map[string]any{"email": email, "reason": "daily_quota"}))
		http.Error(w, "daily verification-code quota exceeded", http.StatusTooManyRequests)
		return
	}
	err := h.svc.RequestEmailResetCode(r.Context(), email, h.provider)
	switch {
	case errors.Is(err, ErrInvalidEmail):
		http.Error(w, "invalid email", http.StatusBadRequest)
	case errors.Is(err, ErrOTPTooSoon):
		http.Error(w, "code sent too recently", http.StatusTooManyRequests)
	case err != nil:
		h.log.Warn("email reset code request failed", "err", err)
		http.Error(w, "failed to send code", http.StatusInternalServerError)
	default:
		h.throttle.RecordSend(email, ip)
		h.record(audit.Success(audit.ActionEmailResetCodeRequest).FromRequest(r).
			Detail(map[string]any{"email": email}))
		w.WriteHeader(http.StatusNoContent)
	}
}

// serveResetPassword verifies the code and sets a new password for the
// account holding the email.
func (h *EmailResetHandler) serveResetPassword(w http.ResponseWriter, r *http.Request) {
	var req emailResetRequest
	if !readAuthBody(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	if !validateEmailForReset(email) {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}
	ip := audit.ClientIP(r)
	// Same (email, ip) verify throttle as the phone reset: wrong codes lock
	// the pair, so an attacker who steals one code cannot grind the 6-digit
	// space against the recovery path.
	if allowed, retryAfter := h.throttle.CheckVerify(email, ip); !allowed {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	err := h.svc.ResetPasswordByEmail(r.Context(), email, req.Code, req.Password)
	if err != nil {
		// Only a genuinely wrong code counts as a failed guess; a DB hiccup,
		// an unknown email, or a weak password must not lock the pair.
		if errors.Is(err, ErrInvalidCode) {
			h.throttle.FailVerify(email, ip)
		}
		h.record(audit.Failure(audit.ActionEmailPasswordReset).FromRequest(r).
			Detail(map[string]any{"email": email}))
		switch {
		case errors.Is(err, ErrInvalidCode):
			http.Error(w, "invalid verification code", http.StatusUnauthorized)
		case errors.Is(err, ErrWeakPassword):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrNoAccountForEmail):
			http.Error(w, "no account for this email", http.StatusNotFound)
		case errors.Is(err, ErrNoPasswordForAccount):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrUserDisabled):
			http.Error(w, "account disabled", http.StatusForbidden)
		default:
			h.log.Warn("email password reset failed", "err", err)
			http.Error(w, "password reset failed", http.StatusInternalServerError)
		}
		return
	}
	h.throttle.SuccessVerify(email, ip)
	h.record(audit.Success(audit.ActionEmailPasswordReset).FromRequest(r).
		Detail(map[string]any{"email": email}))
	w.WriteHeader(http.StatusNoContent)
}

// record writes one event to the audit trail when one is wired. It is a no-op
// otherwise, and never affects the response.
func (h *EmailResetHandler) record(e audit.Event) {
	if h.audit != nil {
		h.audit.LogAndReport(context.Background(), e)
	}
}
