package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"nowhere-agent/internal/audit"
)

// PhoneHandler serves the phone/OTP auth routes: POST /api/auth/phone/
// request-code and POST /api/auth/phone/verify. Both are OPEN routes (no
// bearer token — the caller has none yet). When no SMS provider is wired the
// handler is not registered at all (the login page hides the phone tab).
type PhoneHandler struct {
	svc      *Service
	provider SMSProvider
	throttle *OTPThrottler
	audit    *audit.Logger
	log      *slog.Logger
	now      func() time.Time
}

// NewPhoneHandler builds the phone-auth handler over the service and the
// configured SMS provider.
func NewPhoneHandler(svc *Service, provider SMSProvider) *PhoneHandler {
	return &PhoneHandler{
		svc: svc, provider: provider, throttle: NewOTPThrottler(), log: slog.Default(),
		now: func() time.Time { return time.Now().UTC() },
	}
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
	mux.HandleFunc("GET /api/auth/phone/enabled", h.serveEnabled)
}

type phoneRequest struct {
	Phone string `json:"phone"`
}

type phoneVerifyRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

// serveEnabled lets the login page probe whether phone login exists (404 when
// not wired), mirroring the OIDC probe.
func (h *PhoneHandler) serveEnabled(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (h *PhoneHandler) serveRequestCode(w http.ResponseWriter, r *http.Request) {
	var req phoneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
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
		h.throttle.FailVerify(phone, ip)
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

// record writes one event to the audit trail when one is wired. It is a no-op
// otherwise, and never affects the response.
func (h *PhoneHandler) record(e audit.Event) {
	if h.audit != nil {
		h.audit.LogAndReport(context.Background(), e)
	}
}
