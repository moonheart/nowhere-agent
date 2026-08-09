package adminapi

import (
	"net/http"
	"strings"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/usage"
)

// Team-tier routes (/api/teams/**). The role guard runs in the middleware
// (requireTeamRole); what remains here is the work plus the few checks that
// depend on WHO the target is, not just what role the caller holds.

type teamDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role,omitempty"`
	Members   int       `json:"members,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type memberDTO struct {
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Disabled    bool      `json:"disabled"`
	JoinedAt    time.Time `json:"joined_at"`
}

func memberDTOs(members []identity.TeamMember) []memberDTO {
	out := make([]memberDTO, 0, len(members))
	for _, m := range members {
		out = append(out, memberDTO{
			UserID:      m.UserID,
			Email:       m.Email,
			DisplayName: m.DisplayName,
			Role:        string(m.Role),
			Disabled:    m.Disabled,
			JoinedAt:    m.JoinedAt,
		})
	}
	return out
}

func (h *Handler) myTeams(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	teams, err := h.identity.TeamsForUser(r.Context(), u.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]teamDTO, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamDTO{
			ID:        t.Team.ID,
			Name:      t.Team.Name,
			Role:      string(t.Role),
			CreatedAt: t.Team.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": out})
}

type createTeamRequest struct {
	Name string `json:"name"`
	// OwnerUserID is only honored on the platform route; on /api/teams the
	// creator is always the owner.
	OwnerUserID string `json:"owner_user_id"`
}

func (h *Handler) createTeam(w http.ResponseWriter, r *http.Request) {
	var req createTeamRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "team name required")
		return
	}
	u := caller(r)
	team, err := h.identity.CreateTeam(r.Context(), name, u.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamCreate).Target("team", team.ID).Detail(map[string]any{"name": team.Name}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"team": teamDTO{ID: team.ID, Name: team.Name, Role: string(identity.RoleOwner), CreatedAt: team.CreatedAt},
	})
}

func (h *Handler) getTeam(w http.ResponseWriter, r *http.Request) {
	team, err := h.identity.TeamByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	dto := teamDTO{ID: team.ID, Name: team.Name, CreatedAt: team.CreatedAt}
	if role, ok := h.callerRoleInTeam(r, team.ID); ok {
		dto.Role = string(role)
	}
	writeJSON(w, http.StatusOK, map[string]any{"team": dto})
}

type renameTeamRequest struct {
	Name string `json:"name"`
}

func (h *Handler) renameTeam(w http.ResponseWriter, r *http.Request) {
	var req renameTeamRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "team name required")
		return
	}
	if err := h.identity.RenameTeam(r.Context(), r.PathValue("id"), name); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamRename).Target("team", r.PathValue("id")).Detail(map[string]any{"name": name}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteTeam(w http.ResponseWriter, r *http.Request) {
	if err := h.identity.DeleteTeam(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamDelete).Target("team", r.PathValue("id")))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	members, err := h.identity.ListMembers(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": memberDTOs(members)})
}

type addMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	var req addMemberRequest
	if !decode(w, r, &req) {
		return
	}
	role := identity.Role(req.Role)
	if req.Role == "" {
		role = identity.RoleMember
	}
	// A team admin must not be able to mint an owner: that would let them
	// escalate past the role they hold. Only an owner (or a platform admin)
	// can create another owner.
	if role == identity.RoleOwner && !h.callerMayGrantOwner(r) {
		writeError(w, http.StatusForbidden, "only a team owner can add another owner")
		return
	}
	member, err := h.identity.AddMemberByEmail(r.Context(), r.PathValue("id"), strings.TrimSpace(req.Email), role)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamMemberAdd).Target("team", r.PathValue("id")).Detail(map[string]any{"user_id": member.UserID, "email": member.Email, "role": string(role)}))
	writeJSON(w, http.StatusCreated, map[string]any{"member": memberDTOs([]identity.TeamMember{member})[0]})
}

// callerMayGrantOwner reports whether the caller can hand out the owner role:
// a platform administrator always can, a team member only if they are an owner.
func (h *Handler) callerMayGrantOwner(r *http.Request) bool {
	if caller(r).IsAdmin() {
		return true
	}
	role, ok := h.callerRoleInTeam(r, r.PathValue("id"))
	return ok && role == identity.RoleOwner
}

type changeRoleRequest struct {
	Role string `json:"role"`
}

func (h *Handler) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	var req changeRoleRequest
	if !decode(w, r, &req) {
		return
	}
	if err := h.identity.ChangeMemberRole(r.Context(), r.PathValue("id"), r.PathValue("userId"), identity.Role(req.Role)); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamMemberRole).Target("team", r.PathValue("id")).Detail(map[string]any{"user_id": r.PathValue("userId"), "role": req.Role}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("id")
	targetID := r.PathValue("userId")
	u := caller(r)

	// The route admits any member, because leaving is removing yourself.
	// Removing SOMEONE ELSE needs the admin role in the team (or platform
	// admin) — the middleware only established membership.
	if targetID != u.ID && !u.IsAdmin() {
		role, ok := h.callerRoleInTeam(r, teamID)
		if !ok || !role.AtLeast(identity.RoleAdmin) {
			writeError(w, http.StatusForbidden, "requires team role admin or higher")
			return
		}
	}
	if err := h.identity.RemoveMember(r.Context(), teamID, targetID); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamMemberRemove).Target("team", teamID).Detail(map[string]any{"user_id": targetID, "self": targetID == u.ID}))
	w.WriteHeader(http.StatusNoContent)
}

// ---- team usage and memories ----

func (h *Handler) teamUsage(w http.ResponseWriter, r *http.Request) {
	if h.usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage reporting unavailable")
		return
	}
	teamID := r.PathValue("id")
	rng := usage.Range{From: timeParam(r, "from"), To: timeParam(r, "to")}

	total, err := h.usage.ForTeam(r.Context(), teamID, rng)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	daily, err := h.usage.DailyForTeam(r.Context(), teamID, rng)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total,
		"daily": rowsOf(daily),
		// The approximation travels with the numbers, so a client cannot render
		// them as an exact partition without ignoring the payload.
		"approximate": true,
		"note":        usage.TeamOverlapNote,
	})
}

func (h *Handler) teamMemories(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	mems, err := h.memories.ListByScope(r.Context(), identity.TeamScope(r.PathValue("id")))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": memoryDTOs(mems)})
}

func (h *Handler) deleteTeamMemory(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	mid := r.PathValue("mid")
	if !h.authorizeMemoryScope(w, r, mid, identity.TeamScope(r.PathValue("id"))) {
		return
	}
	if err := h.memories.Forget(r.Context(), mid); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMemoryDelete).Target("memory", mid).Detail(map[string]any{"team_id": r.PathValue("id")}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deprecateTeamMemory(w http.ResponseWriter, r *http.Request) {
	if h.memories == nil {
		writeError(w, http.StatusServiceUnavailable, "memory unavailable")
		return
	}
	mid := r.PathValue("mid")
	if !h.authorizeMemoryScope(w, r, mid, identity.TeamScope(r.PathValue("id"))) {
		return
	}
	if err := h.memories.Deprecate(r.Context(), mid); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMemoryDeprecate).Target("memory", mid).Detail(map[string]any{"team_id": r.PathValue("id")}))
	w.WriteHeader(http.StatusNoContent)
}
