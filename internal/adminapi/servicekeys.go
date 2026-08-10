package adminapi

import (
	"net/http"
	"strings"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/identity"
)

// Service-key routes (enterprise integration): long-lived programmatic
// credentials that let external systems call the agent API without a 30-day
// user session. All sit behind requireAdmin — a service key inherits its
// owner's permissions and outlives the issuing admin, so issuing/revoking is a
// platform-administration act. The raw token is returned exactly once, at
// creation; only its hash is stored.

// serviceKeyDTO is the wire shape of one service key. The raw token field is
// populated ONLY on create.
type serviceKeyDTO struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	UserID     string  `json:"user_id"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at"`
	LastUsedAt *string `json:"last_used_at"`
	RevokedAt  *string `json:"revoked_at"`
	Token      string  `json:"token,omitempty"`
}

func serviceKeyDTOOf(k identity.ServiceKey) serviceKeyDTO {
	d := serviceKeyDTO{
		ID:        k.ID,
		Name:      k.Name,
		UserID:    k.UserID,
		CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339),
	}
	if k.ExpiresAt != nil {
		s := k.ExpiresAt.UTC().Format(time.RFC3339)
		d.ExpiresAt = &s
	}
	if k.LastUsedAt != nil {
		s := k.LastUsedAt.UTC().Format(time.RFC3339)
		d.LastUsedAt = &s
	}
	if k.RevokedAt != nil {
		s := k.RevokedAt.UTC().Format(time.RFC3339)
		d.RevokedAt = &s
	}
	return d
}

// listServiceKeys lists keys: GET /api/admin/service-keys?user_id=&revoked=.
// Without user_id every key on the platform is listed; revoked=1 includes
// revoked keys (default: active only).
func (h *Handler) listServiceKeys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	userID := strings.TrimSpace(q.Get("user_id"))
	includeRevoked := q.Get("revoked") == "1" || q.Get("revoked") == "true"
	keys, err := h.identity.ListServiceKeys(r.Context(), userID, includeRevoked)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]serviceKeyDTO, 0, len(keys))
	for _, k := range keys {
		out = append(out, serviceKeyDTOOf(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"service_keys": out})
}

type createServiceKeyRequest struct {
	Name    string `json:"name"`
	UserID  string `json:"user_id"`
	// TTLDays bounds the key's lifetime; 0 or omitted means never expires.
	// A non-expiring key is the point of programmatic access — the external
	// system must not need a 30-day re-issue cycle — so expiry is opt-in.
	TTLDays int `json:"ttl_days"`
}

// createServiceKey issues one key: POST /api/admin/service-keys. The raw token
// is returned once in the response and never again (not stored, not logged).
func (h *Handler) createServiceKey(w http.ResponseWriter, r *http.Request) {
	var req createServiceKeyRequest
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if strings.TrimSpace(req.UserID) == "" {
		writeError(w, http.StatusBadRequest, "user_id required")
		return
	}
	if req.TTLDays < 0 {
		writeError(w, http.StatusBadRequest, "ttl_days must be non-negative")
		return
	}
	var ttl time.Duration
	if req.TTLDays > 0 {
		ttl = time.Duration(req.TTLDays) * 24 * time.Hour
	}
	raw, key, err := h.identity.CreateServiceKey(r.Context(), req.Name, req.UserID, ttl)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	// The audit record names the key and its owner — never the token material.
	h.record(r, audit.Success(audit.ActionServiceKeyCreate).
		Target("service_key", key.ID).
		Detail(map[string]any{"name": req.Name, "user_id": req.UserID, "ttl_days": req.TTLDays}))
	d := serviceKeyDTOOf(key)
	d.Token = raw
	writeJSON(w, http.StatusCreated, map[string]any{"service_key": d})
}

// revokeServiceKey invalidates one key: DELETE /api/admin/service-keys/{id}.
// Soft delete (revoked_at) keeps the audit trail; a revoked key never
// authenticates again.
func (h *Handler) revokeServiceKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.identity.RevokeServiceKey(r.Context(), id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionServiceKeyRevoke).Target("service_key", id))
	w.WriteHeader(http.StatusNoContent)
}
