package inbound

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/webhook"
)

const (
	// maxBodyBytes bounds a trigger payload (metadata + prompt) at 256 KiB.
	maxBodyBytes = 256 << 10
	// signatureWindow bounds replay: X-Nowhere-Timestamp must be within
	// signatureWindow of the server clock.
	signatureWindow = 5 * time.Minute
	secretPrefix    = "wh_"
)

// Handler serves the public trigger endpoint and the authed management API.
type Handler struct {
	store      *Store
	dispatcher *Dispatcher
	audit      *audit.Logger
	// urlGuard, when set, applies the outbound webhook SSRF guard to
	// notify_url at write time (the delivery-time guard in the notifier is
	// the backstop). Nil keeps the basic http(s) scheme check.
	urlGuard *webhook.Guard
	log      *slog.Logger
	now      func() time.Time
}

// NewHandler creates an inbound Handler over the store and dispatcher.
func NewHandler(st *Store, d *Dispatcher) *Handler {
	return &Handler{
		store: st, dispatcher: d, log: slog.Default(),
		now: func() time.Time { return time.Now().UTC() },
	}
}

// WithAudit wires the audit trail (best-effort, never fails the action).
func (h *Handler) WithAudit(l *audit.Logger) *Handler {
	h.audit = l
	return h
}

// WithURLGuard wires the outbound webhook SSRF guard into notify_url write
// validation.
func (h *Handler) WithURLGuard(g *webhook.Guard) *Handler {
	h.urlGuard = g
	return h
}

// SetLogger overrides the handler's logger.
func (h *Handler) SetLogger(l *slog.Logger) *Handler {
	if l != nil {
		h.log = l
	}
	return h
}

// SetClock overrides the handler's clock (tests).
func (h *Handler) SetClock(now func() time.Time) *Handler {
	if now != nil {
		h.now = now
	}
	return h
}

// RegisterPublic mounts the trigger endpoint on the unauthenticated mux. The
// route itself authenticates via the webhook's HMAC signature.
func (h *Handler) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/inbound/{id}", h.serveTrigger)
}

// RegisterAuthed mounts the self-service management routes onto the protected
// group (the group's auth middleware runs before these handlers).
func (h *Handler) RegisterAuthed(g *httpx.Router) {
	g.HandleFunc("GET /api/me/inbound", h.serveList)
	g.HandleFunc("POST /api/me/inbound", h.serveCreate)
	g.HandleFunc("PATCH /api/me/inbound/{id}", h.serveToggle)
	g.HandleFunc("DELETE /api/me/inbound/{id}", h.serveDelete)
	g.HandleFunc("POST /api/me/inbound/{id}/rotate", h.serveRotate)
}

// ---- trigger ----

// triggerPayload is the run request an external system POSTs.
type triggerPayload struct {
	Prompt   string         `json:"prompt"`
	Metadata map[string]any `json:"metadata"`
}

// serveTrigger handles POST /api/inbound/{id}. It verifies the HMAC signature,
// then hands the payload to the dispatcher. The run executes asynchronously;
// the response carries the run and session ids so the caller can correlate
// with the outbound completion notification.
func (h *Handler) serveTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wh, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		// A missing webhook is indistinguishable from a wrong secret from the
		// caller's side; answer uniformly.
		http.Error(w, "invalid or disabled webhook", http.StatusUnauthorized)
		return
	}
	if !wh.Enabled {
		http.Error(w, "invalid or disabled webhook", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifySignature(r, wh.Secret, body, h.now()) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	// Replay guard: the nonce is folded into the signature and deduplicated,
	// so the same signed event can only start one run within the window.
	if err := h.store.ClaimNonce(r.Context(), wh.ID, r.Header.Get("X-Nowhere-Nonce"), h.now()); err != nil {
		if errors.Is(err, ErrReplay) {
			http.Error(w, "replayed nonce", http.StatusConflict)
			return
		}
		http.Error(w, "dedupe failed", http.StatusInternalServerError)
		return
	}

	var p triggerPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(p.Prompt) == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}

	// Best-effort usage stamp; a failed stamp must not fail the trigger.
	if err := h.store.TouchLastUsed(r.Context(), wh.ID); err != nil {
		h.log.Warn("inbound: touch last_used failed", "webhook", wh.ID, "err", err)
	}

	runID, sessionID, err := h.dispatcher.Dispatch(r.Context(), wh, p.Prompt, p.Metadata)
	if err != nil {
		h.log.Warn("inbound: trigger failed", "webhook", wh.ID, "err", err)
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, ErrDisabled):
			status = http.StatusUnauthorized
		case errors.Is(err, ErrPendingInteraction):
			status = http.StatusConflict
		case errors.Is(err, ErrNotOwner):
			status = http.StatusForbidden
		case errors.Is(err, quota.ErrBudgetExceeded):
			status = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), status)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id":     runID,
		"session_id": sessionID,
		"status":     "started",
	})
}

// verifySignature checks X-Nowhere-Timestamp + X-Nowhere-Nonce +
// X-Nowhere-Signature. The signature is HMAC-SHA256 over
// "<ts>.<nonce>.<body>" with the webhook's plaintext secret (decrypted at
// read time from its encrypted-at-rest envelope); the nonce is required and
// deduplicated by the store, which bounds replay.
func verifySignature(r *http.Request, secret string, body []byte, now time.Time) bool {
	tsStr := r.Header.Get("X-Nowhere-Timestamp")
	nonce := r.Header.Get("X-Nowhere-Nonce")
	sig := r.Header.Get("X-Nowhere-Signature")
	if tsStr == "" || nonce == "" || sig == "" || len(nonce) > 128 {
		return false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	if delta := now.Unix() - ts; delta < -int64(signatureWindow/time.Second) || delta > int64(signatureWindow/time.Second) {
		return false
	}
	expected := hmacSHA256Hex(secret, tsStr+"."+nonce+"."+string(body))
	// Constant-time compare against the received signature, whatever its form.
	got := strings.TrimPrefix(sig, "sha256=")
	return hmac.Equal([]byte(got), []byte(expected))
}

// hmacSHA256Hex returns hex(HMAC-SHA256(key, msg)).
func hmacSHA256Hex(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// ---- management ----

type webhookDTO struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	AgentDef        string     `json:"agent_def,omitempty"`
	SystemPrompt    string     `json:"system_prompt,omitempty"`
	TargetSessionID string     `json:"target_session_id,omitempty"`
	NotifyURL       string     `json:"notify_url,omitempty"`
	Enabled         bool       `json:"enabled"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

func dtoOf(w Webhook) webhookDTO {
	return webhookDTO{
		ID: w.ID, Name: w.Name, AgentDef: w.AgentDef,
		SystemPrompt: w.SystemPrompt, TargetSessionID: w.TargetSessionID,
		NotifyURL: w.NotifyURL, Enabled: w.Enabled,
		LastUsedAt: w.LastUsedAt, CreatedAt: w.CreatedAt,
	}
}

// createRequest is the management create payload. agent_def and system_prompt
// are mutually exclusive (an agent def supplies both prompt and model).
type createRequest struct {
	Name            string `json:"name"`
	AgentDef        string `json:"agent_def"`
	SystemPrompt    string `json:"system_prompt"`
	TargetSessionID string `json:"target_session_id"`
	NotifyURL       string `json:"notify_url"`
}

func (h *Handler) serveList(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	whs, err := h.store.ListByUser(r.Context(), u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]webhookDTO, 0, len(whs))
	for _, wh := range whs {
		out = append(out, dtoOf(wh))
	}
	writeJSON(w, http.StatusOK, map[string]any{"inbound_webhooks": out})
}

func (h *Handler) serveCreate(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	var req createRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.AgentDef != "" && req.SystemPrompt != "" {
		http.Error(w, "agent_def and system_prompt are mutually exclusive", http.StatusBadRequest)
		return
	}
	if req.NotifyURL != "" && !validNotifyURL(req.NotifyURL, h.urlGuard) {
		http.Error(w, "notify_url must be an http(s) URL", http.StatusBadRequest)
		return
	}
	secret, err := generateSecret()
	if err != nil {
		http.Error(w, "generate secret", http.StatusInternalServerError)
		return
	}
	wh, err := h.store.Create(r.Context(), Webhook{
		Name:            req.Name,
		UserID:          u.ID,
		Secret:          secret,
		AgentDef:        req.AgentDef,
		SystemPrompt:    req.SystemPrompt,
		TargetSessionID: req.TargetSessionID,
		NotifyURL:       req.NotifyURL,
		Enabled:         true,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.record(r, audit.Success(audit.ActionInboundWebhookCreate).Target("inbound_webhook", wh.ID))
	writeJSON(w, http.StatusCreated, map[string]any{
		"inbound_webhook": dtoOf(wh),
		// The plaintext secret is only ever visible here; store only its hash.
		"secret": secret,
	})
}

func (h *Handler) serveToggle(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := h.store.SetEnabled(r.Context(), id, u.ID, req.Enabled); err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.record(r, audit.Success(audit.ActionInboundWebhookToggle).Target("inbound_webhook", id).Detail(map[string]any{"enabled": req.Enabled}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) serveDelete(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	id := r.PathValue("id")
	ok, err := h.store.Delete(r.Context(), id, u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.record(r, audit.Success(audit.ActionInboundWebhookDelete).Target("inbound_webhook", id))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) serveRotate(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	id := r.PathValue("id")
	secret, err := generateSecret()
	if err != nil {
		http.Error(w, "generate secret", http.StatusInternalServerError)
		return
	}
	if err := h.store.RotateSecret(r.Context(), id, u.ID, secret); err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.record(r, audit.Success(audit.ActionInboundWebhookRotate).Target("inbound_webhook", id))
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret})
}

// record writes one event to the audit trail when one is wired (no-op
// otherwise); never affects the response.
func (h *Handler) record(r *http.Request, e audit.Event) {
	if h.audit == nil {
		return
	}
	u := caller(r)
	h.audit.LogAndReport(r.Context(), e.FromRequest(r).Actor(u.ID, u.Email))
}

func caller(r *http.Request) identity.User {
	if u, ok := identity.UserFromContext(r.Context()); ok {
		return u
	}
	return identity.User{}
}

// generateSecret returns an opaque wh_-prefixed trigger secret.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretPrefix + hex.EncodeToString(b), nil
}

// validNotifyURL accepts http(s) URLs with a host — the SSRF guard for
// outbound delivery lives in the notifier; when one is wired it also screens
// private/loopback targets at write time.
func validNotifyURL(u string, g *webhook.Guard) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	if g != nil {
		return g.ValidateURL(u) == nil
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
