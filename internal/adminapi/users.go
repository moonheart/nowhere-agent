package adminapi

import (
	"net/http"
	"strings"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/usage"
)

// Platform-tier routes (/api/admin/**), all behind requireAdmin. The lock-out
// guards (no self-demote, self-disable, self-delete) live in identity.Service
// so they cannot be bypassed by a second caller; here they surface as 409.

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	s, err := h.identity.Stats(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := map[string]any{"users": s.Users, "admins": s.Admins, "teams": s.Teams}
	if h.usage != nil {
		if t, err := h.usage.Totals(r.Context(), usage.Range{}); err == nil {
			out["usage"] = t
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := intParam(r, "limit", 50, 200)
	offset := intParam(r, "offset", 0, 0)

	users, total, err := h.identity.ListUsers(r.Context(), q, limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users":  userDTOs(users),
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

type createUserRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decode(w, r, &req) {
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		writeError(w, http.StatusBadRequest, "email required")
		return
	}
	if len(req.Password) < minPasswordLen {
		writeError(w, http.StatusBadRequest, shortPasswordMsg)
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		name = email
	}
	u, err := h.identity.CreateAccount(r.Context(), email, req.Password, name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": userDTOOf(u)})
}

type patchUserRequest struct {
	DisplayName  *string `json:"display_name"`
	PlatformRole *string `json:"platform_role"`
	Disabled     *bool   `json:"disabled"`
}

func (h *Handler) patchUser(w http.ResponseWriter, r *http.Request) {
	var req patchUserRequest
	if !decode(w, r, &req) {
		return
	}
	actor := caller(r)
	targetID := r.PathValue("id")

	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" {
			writeError(w, http.StatusBadRequest, "display name cannot be empty")
			return
		}
		if err := h.identity.UpdateDisplayName(r.Context(), targetID, name); err != nil {
			writeServiceError(w, err)
			return
		}
	}
	if req.PlatformRole != nil {
		if err := h.identity.SetPlatformRole(r.Context(), actor.ID, targetID, identity.PlatformRole(*req.PlatformRole)); err != nil {
			writeServiceError(w, err)
			return
		}
	}
	if req.Disabled != nil {
		if err := h.identity.SetUserDisabled(r.Context(), actor.ID, targetID, *req.Disabled); err != nil {
			writeServiceError(w, err)
			return
		}
	}

	fresh, err := h.identity.UserByID(r.Context(), targetID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userDTOOf(fresh)})
}

type resetPasswordRequest struct {
	Password string `json:"password"`
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Password) < minPasswordLen {
		writeError(w, http.StatusBadRequest, shortPasswordMsg)
		return
	}
	if err := h.identity.ResetPassword(r.Context(), r.PathValue("id"), req.Password); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	actor := caller(r)
	if err := h.identity.DeleteAccount(r.Context(), actor.ID, r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listAllTeams(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := intParam(r, "limit", 50, 200)
	offset := intParam(r, "offset", 0, 0)

	teams, total, err := h.identity.ListTeams(r.Context(), q, limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]teamDTO, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamDTO{
			ID:        t.Team.ID,
			Name:      t.Team.Name,
			Members:   t.MemberCount,
			CreatedAt: t.Team.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": out, "total": total, "limit": limit, "offset": offset})
}

// createTeamForOwner lets an administrator create a team owned by someone else
// — the platform-tier counterpart of POST /api/teams, which always makes the
// caller the owner.
func (h *Handler) createTeamForOwner(w http.ResponseWriter, r *http.Request) {
	var req createTeamRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "team name required")
		return
	}
	ownerID := strings.TrimSpace(req.OwnerUserID)
	if ownerID == "" {
		ownerID = caller(r).ID
	} else if _, err := h.identity.UserByID(r.Context(), ownerID); err != nil {
		// Without this the team would be created with a membership row that
		// fails its foreign key, or worse, an owner nobody can sign in as.
		writeServiceError(w, err)
		return
	}
	team, err := h.identity.CreateTeam(r.Context(), name, ownerID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"team": teamDTO{ID: team.ID, Name: team.Name, Members: 1, CreatedAt: team.CreatedAt},
	})
}

func (h *Handler) platformUsage(w http.ResponseWriter, r *http.Request) {
	if h.usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage reporting unavailable")
		return
	}
	rng := usage.Range{From: timeParam(r, "from"), To: timeParam(r, "to")}
	limit := intParam(r, "limit", 100, 500)

	total, err := h.usage.Totals(r.Context(), rng)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	daily, err := h.usage.DailyTotals(r.Context(), rng)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	out := map[string]any{"total": total, "daily": rowsOf(daily)}
	switch r.URL.Query().Get("group_by") {
	case "team":
		rows, err := h.usage.ByTeam(r.Context(), rng, limit)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		out["rows"] = rowsOf(rows)
		out["group_by"] = "team"
		out["approximate"] = true
		out["note"] = usage.TeamOverlapNote
	default:
		rows, err := h.usage.ByUser(r.Context(), rng, limit)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		out["rows"] = rowsOf(rows)
		out["group_by"] = "user"
	}
	writeJSON(w, http.StatusOK, out)
}

// adminMemories lists memories in any scope. Scope is explicit rather than
// inferred so an administrator cannot accidentally sweep every user's private
// memories into one view.
func (h *Handler) adminMemories(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	scope, ok := scopeFromQuery(w, r)
	if !ok {
		return
	}
	mems, err := h.memories.ListByScope(r.Context(), scope)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": memoryDTOs(mems)})
}

// scopeFromQuery builds a ScopeRef from ?scope=&user_id=&team_id=, answering
// the response itself on a malformed combination.
func scopeFromQuery(w http.ResponseWriter, r *http.Request) (identity.ScopeRef, bool) {
	q := r.URL.Query()
	switch identity.Scope(q.Get("scope")) {
	case identity.ScopeSystem, "":
		return identity.SystemScope(), true
	case identity.ScopeUser:
		id := q.Get("user_id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "user_id required for scope=user")
			return identity.ScopeRef{}, false
		}
		return identity.UserScope(id), true
	case identity.ScopeTeam:
		id := q.Get("team_id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "team_id required for scope=team")
			return identity.ScopeRef{}, false
		}
		return identity.TeamScope(id), true
	default:
		writeError(w, http.StatusBadRequest, "scope must be user, team, or system")
		return identity.ScopeRef{}, false
	}
}

func (h *Handler) adminDeleteMemory(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	// A platform administrator is entitled to every scope, so there is nothing
	// to check beyond existence — which GetByID gives us, turning a delete of a
	// non-existent id into a 404 instead of a silent success.
	if _, err := h.memories.GetByID(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.memories.Forget(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) adminDeprecateMemory(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	if _, err := h.memories.GetByID(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.memories.Deprecate(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
