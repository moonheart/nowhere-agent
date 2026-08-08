package adminapi

import (
	"net/http"
	"strings"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/usage"
)

// Self-service routes (/api/me/**). They need no tier guard beyond
// authentication: every one of them resolves its target from the authenticated
// caller rather than from the request, so there is nothing to authorize.
// GET /api/me itself stays in the identity package, which owns the profile.

type updateMeRequest struct {
	DisplayName *string `json:"display_name"`
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) {
	var req updateMeRequest
	if !decode(w, r, &req) {
		return
	}
	u := caller(r)
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" {
			writeError(w, http.StatusBadRequest, "display name cannot be empty")
			return
		}
		if err := h.identity.UpdateDisplayName(r.Context(), u.ID, name); err != nil {
			writeServiceError(w, err)
			return
		}
	}
	fresh, err := h.identity.UserByID(r.Context(), u.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userDTOOf(fresh)})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.NewPassword) < minPasswordLen {
		writeError(w, http.StatusBadRequest, shortPasswordMsg)
		return
	}
	u := caller(r)
	if err := h.identity.ChangePassword(r.Context(), u.ID, req.CurrentPassword, req.NewPassword); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMePasswordChange).Target("user", u.ID))
	// Every token was revoked, including this request's — say so, so the client
	// signs the user back in instead of silently 401-ing on the next call.
	writeJSON(w, http.StatusOK, map[string]any{
		"reauthenticate": true,
		"message":        "password changed; all sessions were signed out",
	})
}

func (h *Handler) myUsage(w http.ResponseWriter, r *http.Request) {
	if h.usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage reporting unavailable")
		return
	}
	u := caller(r)
	rng := usage.Range{From: timeParam(r, "from"), To: timeParam(r, "to")}

	total, err := h.usage.ForUser(r.Context(), u.ID, rng)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	daily, err := h.usage.DailyForUser(r.Context(), u.ID, rng)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "daily": rowsOf(daily)})
}

func (h *Handler) myMemories(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	u := caller(r)
	mems, err := h.memories.ListByScope(r.Context(), identity.UserScope(u.ID))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": memoryDTOs(mems)})
}

func (h *Handler) deleteMyMemory(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	u := caller(r)
	// Resolve first and check the scope: Forget takes a bare id, so without
	// this an account could delete anyone's memory by guessing a uuid.
	if !h.authorizeMemoryScope(w, r, r.PathValue("id"), identity.UserScope(u.ID)) {
		return
	}
	if err := h.memories.Forget(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMemoryDelete).Target("memory", r.PathValue("id")))
	w.WriteHeader(http.StatusNoContent)
}

// authorizeMemoryScope resolves a memory and verifies it sits in `want`,
// answering the response itself when it does not. A memory in another scope is
// reported as not-found rather than forbidden, so ids cannot be probed.
func (h *Handler) authorizeMemoryScope(w http.ResponseWriter, r *http.Request, id string, want identity.ScopeRef) bool {
	if id == "" {
		writeError(w, http.StatusBadRequest, "memory id required")
		return false
	}
	m, err := h.memories.GetByID(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return false
	}
	if m.Scope != want {
		writeError(w, http.StatusNotFound, "memory not found")
		return false
	}
	return true
}

type tokenDTO struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Current marks the session making this request, so the UI can label it and
	// avoid offering to revoke the one you are using.
	Current bool `json:"current"`
}

func (h *Handler) myTokens(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	tokens, err := h.identity.ListTokens(r.Context(), u.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	// A failure to resolve the current token is not fatal: the list is still
	// useful, just without the "this device" marker.
	currentID, _ := h.identity.CurrentTokenID(r.Context(), bearerToken(r))

	out := make([]tokenDTO, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenDTO{
			ID:        t.ID,
			CreatedAt: t.CreatedAt,
			ExpiresAt: t.ExpiresAt,
			Current:   t.ID == currentID && currentID != "",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	if err := h.identity.RevokeToken(r.Context(), u.ID, r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMeTokenRevoke).Target("user", u.ID).Detail(map[string]any{"token_id": r.PathValue("id"), "scope": "single"}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) revokeOtherTokens(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	n, err := h.identity.RevokeOtherTokens(r.Context(), u.ID, bearerToken(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMeTokenRevoke).Target("user", u.ID).Detail(map[string]any{"revoked": n, "scope": "others"}))
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

// ---- shared DTOs ----

const (
	minPasswordLen   = 8
	shortPasswordMsg = "password must be at least 8 characters"
)

type userDTO struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"display_name"`
	PlatformRole string     `json:"platform_role"`
	Disabled     bool       `json:"disabled"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func userDTOOf(u identity.User) userDTO {
	role := u.PlatformRole
	if role == "" {
		role = identity.PlatformRoleUser
	}
	return userDTO{
		ID:           u.ID,
		Email:        u.Email,
		DisplayName:  u.DisplayName,
		PlatformRole: string(role),
		Disabled:     u.Disabled(),
		DisabledAt:   u.DisabledAt,
		CreatedAt:    u.CreatedAt,
	}
}

func userDTOs(users []identity.User) []userDTO {
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, userDTOOf(u))
	}
	return out
}

type memoryDTO struct {
	ID         string    `json:"id"`
	Scope      string    `json:"scope"`
	UserID     string    `json:"user_id,omitempty"`
	TeamID     string    `json:"team_id,omitempty"`
	Kind       string    `json:"kind"`
	Content    string    `json:"content"`
	Deprecated bool      `json:"deprecated"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func memoryDTOs(mems []memory.Memory) []memoryDTO {
	out := make([]memoryDTO, 0, len(mems))
	for _, m := range mems {
		out = append(out, memoryDTO{
			ID:         m.ID,
			Scope:      string(m.Scope.Scope),
			UserID:     m.Scope.UserID,
			TeamID:     m.Scope.TeamID,
			Kind:       string(m.Kind),
			Content:    m.Content,
			Deprecated: m.Deprecated,
			CreatedAt:  m.CreatedAt,
			UpdatedAt:  m.UpdatedAt,
		})
	}
	return out
}

// rowsOf normalizes a nil slice to an empty one so the JSON is always an array
// — a client charting `null` has to special-case it, `[]` it does not.
func rowsOf(rows []usage.Row) []usage.Row {
	if rows == nil {
		return []usage.Row{}
	}
	return rows
}
