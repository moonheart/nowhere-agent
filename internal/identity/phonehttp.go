package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"nowhere-agent/internal/audit"
)

// PhoneHandler serves the phone/OTP auth routes: POST /api/auth/phone/
// request-code and POST /api/auth/phone/verify. Both are OPEN routes (no
// bearer token — the caller has none yet). The handler is always registered;
// whether phone login is AVAILABLE resolves per request from the enabled
// func (the runtime phone_sms_url setting), so the login page's probe and the
// code request both follow the current channel.
type PhoneHandler struct {
	svc      *Service
	provider SMSProvider
	// enabledFor, when set, reports whether phone login is currently
	// available; nil keeps it always on (tests).
	enabledFor func() bool
	throttle   *OTPThrottler
	audit      *audit.Logger
	log        *slog.Logger
	now        func() time.Time
}

// NewPhoneHandler builds the phone-auth handler over the service and the
// configured SMS provider.
func NewPhoneHandler(svc *Service, provider SMSProvider) *PhoneHandler {
	return &PhoneHandler{
		svc: svc, provider: provider, throttle: NewOTPThrottler(), log: slog.Default(),
		now: func() time.Time { return time.Now().UTC() },
	}
}

// WithEnabledFunc wires the live availability probe (the login page hides
// the phone tab and the request-code route 404s when it reports false).
func (h *PhoneHandler) WithEnabledFunc(f func() bool) *PhoneHandler {
	h.enabledFor = f
	return h
}

// WithThrottle overrides the built-in OTP throttler, so a caller can share ONE
// instance across the phone-auth routes and the console's phone-binding route
// (a locked (phone, ip) pair then stays locked everywhere).
func (h *PhoneHandler) WithThrottle(t *OTPThrottler) *PhoneHandler {
	h.throttle = t
	return h
}

// WithAudit wires the audit trail (best-effort).
func (h *PhoneHandler) WithAudit(l *audit.Logger) *PhoneHandler {
	h.audit = l
	return h
}

// SetLogger overrides the handler's logger.
func (h *PhoneHandler) SetLogger(l *slog.Logger) *PhoneHandler {
	if l != nil {
		h.log = l
	}
	return h
}

// Register mounts the open routes.
func (h *PhoneHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/phone/request-code", h.serveRequestCode)
	mux.HandleFunc("POST /api/auth/phone/verify", h.serveVerify)
	mux.HandleFunc("POST /api/auth/phone/reset-password", h.serveResetPassword)
	mux.HandleFunc("GET /api/auth/phone/enabled", h.serveEnabled)
}

type phoneRequest struct {
	Phone string `json:"phone"`
}

type phoneVerifyRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

type phoneResetRequest struct {
	Phone    string `json:"phone"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

// serveEnabled lets the login page probe whether phone login exists (404 when
// not available), mirroring the OIDC probe. Availability resolves live from
// the enabled func, so turning phone login off in the admin console hides it
// immediately.
func (h *PhoneHandler) serveEnabled(w http.ResponseWriter, r *http.Request) {
	if h.enabledFor != nil && !h.enabledFor() {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *PhoneHandler) serveRequestCode(w http.ResponseWriter, r *http.Request) {
	if h.enabledFor != nil && !h.enabledFor() {
		http.NotFound(w, r)
		return
	}
	var req phoneRequest
	if !readAuthBody(w, r, &req) {
		return
	}
	phone := NormalizePhone(req.Phone)
	if phone == "" {
		http.Error(w, "invalid phone number", http.StatusBadRequest)
		return
	}
	ip := audit.ClientIP(r)
	// Anti-SMS-bombing: daily quotas per phone and per IP (the per-code 60s
	// cooldown in the service is the fine-grained wall underneath).
	if err := h.throttle.AllowSend(phone, ip); err != nil {
		h.record(audit.Failure(audit.ActionPhoneOTPRequest).FromRequest(r).
			Detail(map[string]any{"phone": maskPhone(phone), "reason": "daily_quota"}))
		http.Error(w, "daily verification-code quota exceeded", http.StatusTooManyRequests)
		return
	}
	err := h.svc.RequestPhoneOTP(r.Context(), phone, h.provider)
	switch {
	case errors.Is(err, ErrInvalidPhone):
		http.Error(w, "invalid phone number", http.StatusBadRequest)
	case errors.Is(err, ErrOTPTooSoon):
		http.Error(w, "code sent too recently", http.StatusTooManyRequests)
	case err != nil:
		h.log.Warn("phone otp request failed", "err", err)
		http.Error(w, "failed to send code", http.StatusInternalServerError)
	default:
		h.throttle.RecordSend(phone, ip)
		h.record(audit.Success(audit.ActionPhoneOTPRequest).FromRequest(r).
			Detail(map[string]any{"phone": maskPhone(phone)}))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *PhoneHandler) serveVerify(w http.ResponseWriter, r *http.Request) {
	var req phoneVerifyRequest
	if !readAuthBody(w, r, &req) {
		return
	}
	phone := NormalizePhone(req.Phone)
	if phone == "" {
		http.Error(w, "invalid phone number", http.StatusBadRequest)
		return
	}
	ip := audit.ClientIP(r)
	// Verify throttle per (phone, ip): a locked pair waits out the retry.
	if allowed, retryAfter := h.throttle.CheckVerify(phone, ip); !allowed {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	token, u, err := h.svc.VerifyPhoneOTP(r.Context(), phone, req.Code)
	if err != nil {
		// Only a genuinely wrong code counts as a failed guess; a DB hiccup or
		// a malformed number must not lock the (phone, ip) pair for 15 minutes.
		if errors.Is(err, ErrInvalidCode) {
			h.throttle.FailVerify(phone, ip)
		}
		h.record(audit.Failure(audit.ActionAuthLogin).FromRequest(r).Detail(map[string]any{"method": "phone"}))
		switch {
		case errors.Is(err, ErrInvalidPhone):
			http.Error(w, "invalid phone number", http.StatusBadRequest)
		case errors.Is(err, ErrInvalidCode):
			http.Error(w, "invalid verification code", http.StatusUnauthorized)
		case errors.Is(err, ErrUserDisabled):
			http.Error(w, "account disabled", http.StatusForbidden)
		default:
			h.log.Warn("phone otp verify failed", "err", err)
			http.Error(w, "verification failed", http.StatusInternalServerError)
		}
		return
	}
	h.throttle.SuccessVerify(phone, ip)
	h.record(audit.Success(audit.ActionAuthLogin).FromRequest(r).Actor(u.ID, u.Email).
		Target("user", u.ID).Detail(map[string]any{"method": "phone", "phone": maskPhone(phone)}))
	writeJSON(w, http.StatusOK, authResponse{Token: token, User: toDTO(u)})
}

func (h *PhoneHandler) serveResetPassword(w http.ResponseWriter, r *http.Request) {
	if h.enabledFor != nil && !h.enabledFor() {
		http.NotFound(w, r)
		return
	}
	var req phoneResetRequest
	if !readAuthBody(w, r, &req) {
		return
	}
	phone := NormalizePhone(req.Phone)
	if phone == "" {
		http.Error(w, "invalid phone number", http.StatusBadRequest)
		return
	}
	ip := audit.ClientIP(r)
	// Same (phone, ip) verify throttle as serveVerify: wrong codes lock the
	// pair, so an attacker who steals one code cannot grind the 6-digit space
	// against the recovery path any more than against login.
	if allowed, retryAfter := h.throttle.CheckVerify(phone, ip); !allowed {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	err := h.svc.ResetPasswordByPhone(r.Context(), phone, req.Code, req.Password)
	if err != nil {
		// Only a genuinely wrong code counts as a failed guess; a DB hiccup,
		// an unbound phone, or a weak password must not lock the pair.
		if errors.Is(err, ErrInvalidCode) {
			h.throttle.FailVerify(phone, ip)
		}
		h.record(audit.Failure(audit.ActionPhonePasswordReset).FromRequest(r).
			Detail(map[string]any{"phone": maskPhone(phone)}))
		switch {
		case errors.Is(err, ErrInvalidPhone):
			http.Error(w, "invalid phone number", http.StatusBadRequest)
		case errors.Is(err, ErrInvalidCode):
			http.Error(w, "invalid verification code", http.StatusUnauthorized)
		case errors.Is(err, ErrWeakPassword):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrNoAccountForPhone):
			http.Error(w, "no account bound to this phone", http.StatusNotFound)
		case errors.Is(err, ErrUserDisabled):
			http.Error(w, "account disabled", http.StatusForbidden)
		default:
			h.log.Warn("phone password reset failed", "err", err)
			http.Error(w, "password reset failed", http.StatusInternalServerError)
		}
		return
	}
	h.throttle.SuccessVerify(phone, ip)
	h.record(audit.Success(audit.ActionPhonePasswordReset).FromRequest(r).
		Detail(map[string]any{"phone": maskPhone(phone)}))
	w.WriteHeader(http.StatusNoContent)
}

// record writes one event to the audit trail when one is wired. It is a no-op
// otherwise, and never affects the response.
func (h *PhoneHandler) record(e audit.Event) {
	if h.audit != nil {
		h.audit.LogAndReport(context.Background(), e)
	}
}
