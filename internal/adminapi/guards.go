package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/providerreg"
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
// without a membership — they administer every team by definition.
//
// A caller who is not a member gets 404, not 403: 403 would confirm the team
// exists, letting anyone enumerate team ids by probing.
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

// callerRoleInTeam reports the caller's own role, for handlers that need to
// distinguish "an administrator acting on a member" from "a member acting on
// themselves". A platform administrator who is not a member has no team role.
func (h *Handler) callerRoleInTeam(r *http.Request, teamID string) (identity.Role, bool) {
	u, ok := identity.UserFromContext(r.Context())
	if !ok {
		return "", false
	}
	role, member, err := h.identity.RoleInTeam(r.Context(), teamID, u.ID)
	if err != nil || !member {
		return "", false
	}
	return role, true
}

// caller returns the authenticated user; handlers behind RegisterAuthed can
// rely on it being present.
func caller(r *http.Request) identity.User {
	u, _ := identity.UserFromContext(r.Context())
	return u
}

// ---- request plumbing ----

// decode reads a JSON body into v, answering 400 and reporting false on
// malformed input.
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

// writeServiceError maps a service error onto a status code. Keeping the
// mapping in one place stops each handler from inventing its own, which is how
// a "not found" starts leaking as a 500 in some corner of the API.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrSelfTarget):
		writeError(w, http.StatusConflict, "you cannot apply this to your own account")
	case errors.Is(err, identity.ErrLastOwner):
		writeError(w, http.StatusConflict, "a team must keep at least one owner")
	case errors.Is(err, identity.ErrUserExists):
		writeError(w, http.StatusConflict, "an account with that email already exists")
	case errors.Is(err, identity.ErrInvalidRole):
		writeError(w, http.StatusBadRequest, "role must be owner, admin, or member")
	case errors.Is(err, identity.ErrInvalidCredentials):
		writeError(w, http.StatusForbidden, "current password is incorrect")
	case errors.Is(err, identity.ErrInvalidToken):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, memory.ErrNotFound):
		writeError(w, http.StatusNotFound, "memory not found")
	case errors.Is(err, providerreg.ErrNotFound):
		writeError(w, http.StatusNotFound, "provider or model not found")
	case errors.Is(err, providerreg.ErrNameConflict):
		writeError(w, http.StatusConflict, "a provider or model with that name already exists")
	case errors.Is(err, providerreg.ErrDefaultConflict):
		writeError(w, http.StatusConflict, "a default is already set")
	case errors.Is(err, providerreg.ErrProviderInUse):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, providerreg.ErrModelInUse):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, providerreg.ErrProviderDisabled):
		writeError(w, http.StatusConflict, "provider or model is disabled")
	case errors.Is(err, providerreg.ErrModelMismatch):
		writeError(w, http.StatusBadRequest, "model does not belong to the provider")
	case identity.IsNotFound(err):
		writeError(w, http.StatusNotFound, "not found")
	default:
		writeError(w, http.StatusInternalServerError, "request failed")
	}
}

// intParam reads a bounded integer query parameter, falling back to def.
func intParam(r *http.Request, name string, def, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	if max > 0 && v > max {
		return max
	}
	return v
}

// timeParam reads an RFC3339 or YYYY-MM-DD query parameter. An unparseable
// value yields the zero time, which every range treats as unbounded — a
// mistyped filter shows more data, never silently zero.
func timeParam(r *http.Request, name string) time.Time {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t
	}
	return time.Time{}
}

// bearerToken extracts the raw token of the current request, so self-service
// routes can tell the caller's own session apart from their other ones.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}
