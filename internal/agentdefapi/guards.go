package agentdefapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
)

// ---- authorization ----

// requireAdmin gates a platform-tier route on platform_role == admin.
func (h *Handler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := identity.UserFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !u.IsAdmin() {
			writeError(w, http.StatusForbidden, "platform administrator role required")
			return
		}
		next(w, r)
	}
}

// requireTeamRole gates a team-tier route on the caller holding at least `min`
// in the team named by the {id} path value. A platform administrator passes
// without a membership. A non-member gets 404, not 403, so team ids cannot be
// enumerated by probing (same contract as the skill console).
func (h *Handler) requireTeamRole(min identity.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := identity.UserFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		teamID := r.PathValue("id")
		if teamID == "" {
			writeError(w, http.StatusBadRequest, "team id required")
			return
		}
		if u.IsAdmin() {
			next(w, r)
			return
		}
		role, member, err := h.identity.RoleInTeam(r.Context(), teamID, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "authorization check failed")
			return
		}
		if !member {
			writeError(w, http.StatusNotFound, "team not found")
			return
		}
		if !role.AtLeast(min) {
			writeError(w, http.StatusForbidden, "requires team role "+string(min)+" or higher")
			return
		}
		next(w, r)
	}
}

// caller returns the authenticated user; handlers behind RegisterAuthed rely on it.
func caller(r *http.Request) identity.User {
	u, _ := identity.UserFromContext(r.Context())
	return u
}

// ---- request plumbing ----

// maxBodyBytes bounds a management request body (agent definitions carry
// prompts and system text) at 1 MiB; larger bodies are rejected with 413
// before decoding.
const maxBodyBytes = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return false
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.JSON(w, status, v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.Error(w, status, msg)
}

// writeStoreError maps a store error onto a status code, keeping the mapping
// in one place so a not-found never leaks as a 500.
func (h *Handler) writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, agentdef.ErrNotFound) {
		writeError(w, http.StatusNotFound, "agent definition not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "request failed")
}

// storeUnavailable answers 503 when the handler has no store wired.
func (h *Handler) storeUnavailable(w http.ResponseWriter) bool {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "agent definition store unavailable")
		return true
	}
	return false
}
