package skillapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/skill"
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
// enumerated by probing (same contract as the admin console).
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

// ---- scope authorization ----

// authorizeSkillScope resolves a skill and verifies it sits in `want`,
// answering the response itself when it does not. A skill in another scope is
// reported not-found rather than forbidden, so ids cannot be probed (same
// contract as adminapi.authorizeMemoryScope).
func (h *Handler) authorizeSkillScope(w http.ResponseWriter, r *http.Request, id string, want identity.ScopeRef) (skill.Skill, bool) {
	if id == "" {
		writeError(w, http.StatusBadRequest, "skill id required")
		return skill.Skill{}, false
	}
	sk, err := h.store.ByID(r.Context(), id)
	if err != nil {
		h.writeStoreError(w, err)
		return skill.Skill{}, false
	}
	if sk.Scope != want {
		writeError(w, http.StatusNotFound, "skill not found")
		return skill.Skill{}, false
	}
	return sk, true
}

// ---- request plumbing ----

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
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

// writeStoreError maps a store error onto a status code, keeping the mapping in
// one place so a not-found never leaks as a 500.
func (h *Handler) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, skill.ErrNotFound):
		writeError(w, http.StatusNotFound, "skill not found")
	case errors.Is(err, skill.ErrConflict):
		writeError(w, http.StatusConflict, "a skill with this name already exists in the destination team")
	default:
		writeError(w, http.StatusInternalServerError, "request failed")
	}
}

// versionParam reads the {v} path value as a positive version number.
func versionParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(r.PathValue("v")))
	if err != nil || v < 1 {
		writeError(w, http.StatusBadRequest, "version must be a positive integer")
		return 0, false
	}
	return v, true
}

// storeUnavailable answers 503 when the handler has no store wired.
func (h *Handler) storeUnavailable(w http.ResponseWriter) bool {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "skill store unavailable")
		return true
	}
	return false
}
