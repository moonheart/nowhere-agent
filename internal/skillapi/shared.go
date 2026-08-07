package skillapi

import (
	"net/http"

	"nowhere-agent/internal/identity"
)

// Shared single-skill operations, parameterized by the owning scope. Every one
// re-verifies the resolved skill sits in the scope before acting, so an id from
// another scope is reported not-found (never read, written, or deleted).

func (h *Handler) getScopedSkill(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, id string) {
	if h.storeUnavailable(w) {
		return
	}
	sk, ok := h.authorizeSkillScope(w, r, id, scope)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": skillDTOOf(sk)})
}

func (h *Handler) updateScopedSkill(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, id string) {
	if h.storeUnavailable(w) {
		return
	}
	// Resolve first: the update applies to THIS skill, so it must already exist
	// in the scope (a PUT to a foreign or missing id is not an upsert).
	if _, ok := h.authorizeSkillScope(w, r, id, scope); !ok {
		return
	}
	var req skillRequest
	if !decode(w, r, &req) {
		return
	}
	sk, ok := req.toSkill(w, scope)
	if !ok {
		return
	}
	saved, err := h.store.Put(r.Context(), sk, caller(r).ID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": skillDTOOf(saved)})
}

func (h *Handler) deleteScopedSkill(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, id string) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeSkillScope(w, r, id, scope); !ok {
		return
	}
	if err := h.store.Delete(r.Context(), id); err != nil {
		h.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) scopedVersions(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, id string) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeSkillScope(w, r, id, scope); !ok {
		return
	}
	vs, err := h.store.Versions(r.Context(), id)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versionDTOs(vs)})
}

func (h *Handler) scopedVersionAt(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, id string) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeSkillScope(w, r, id, scope); !ok {
		return
	}
	v, ok := versionParam(w, r)
	if !ok {
		return
	}
	sk, err := h.store.VersionAt(r.Context(), id, v)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": skillDTOOf(sk)})
}

func (h *Handler) rollbackScopedSkill(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, id string) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeSkillScope(w, r, id, scope); !ok {
		return
	}
	v, ok := versionParam(w, r)
	if !ok {
		return
	}
	sk, err := h.store.Rollback(r.Context(), id, v, caller(r).ID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": skillDTOOf(sk)})
}

// setScopedSkillEnabled enables/disables a skill in the given scope. The scope
// check runs first (a foreign id reads as not-found); the toggle itself writes
// no version, it only flips the agent-resolution gate.
func (h *Handler) setScopedSkillEnabled(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, id string, enabled bool) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeSkillScope(w, r, id, scope); !ok {
		return
	}
	sk, err := h.store.SetEnabled(r.Context(), id, enabled)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": skillDTOOf(sk)})
}

// moveRequest is the move-to-team payload: the destination team.
type moveRequest struct {
	TeamID string `json:"team_id"`
}

// moveMySkillToTeam relocates one of the caller's own user-scope skills into a
// team. Authorization: the caller must be able to WRITE in the destination team
// (team admin) or be a platform admin — a plain member cannot push skills into
// their team, and a non-member cannot target the team at all. The scope check
// above confines the move to the caller's own skills.
func (h *Handler) moveMySkillToTeam(w http.ResponseWriter, r *http.Request, id string) {
	if h.storeUnavailable(w) {
		return
	}
	// Resolve + scope-check first: only the caller's own (user-scope) skill moves.
	if _, ok := h.authorizeSkillScope(w, r, id, identity.UserScope(caller(r).ID)); !ok {
		return
	}
	var req moveRequest
	if !decode(w, r, &req) {
		return
	}
	if req.TeamID == "" {
		writeError(w, http.StatusBadRequest, "team_id required")
		return
	}
	u := caller(r)
	if !u.IsAdmin() {
		role, member, err := h.identity.RoleInTeam(r.Context(), req.TeamID, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "authorization check failed")
			return
		}
		if !member {
			writeError(w, http.StatusNotFound, "team not found")
			return
		}
		if !role.AtLeast(identity.RoleAdmin) {
			writeError(w, http.StatusForbidden, "requires team admin role to move a skill into the team")
			return
		}
	}
	sk, err := h.store.MoveToTeam(r.Context(), id, req.TeamID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": skillDTOOf(sk)})
}
