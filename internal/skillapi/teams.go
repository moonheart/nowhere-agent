package skillapi

import (
	"net/http"

	"nowhere-agent/internal/identity"
)

// Team-tier routes (/api/teams/{id}/skills/**). The role guard runs in the
// middleware (requireTeamRole); what remains scopes the operation to the team
// in the path. Members read, admins write.

func (h *Handler) teamSkills(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	sks, err := h.store.ListByScope(r.Context(), identity.TeamScope(r.PathValue("id")))
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skillDTOs(sks)})
}

func (h *Handler) createTeamSkill(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	var req skillRequest
	if !decode(w, r, &req) {
		return
	}
	sk, ok := req.toSkill(w, identity.TeamScope(r.PathValue("id")))
	if !ok {
		return
	}
	saved, err := h.store.Put(r.Context(), sk, caller(r).ID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"skill": skillDTOOf(saved)})
}

func (h *Handler) getTeamSkill(w http.ResponseWriter, r *http.Request) {
	h.getScopedSkill(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("sid"))
}

func (h *Handler) updateTeamSkill(w http.ResponseWriter, r *http.Request) {
	h.updateScopedSkill(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("sid"))
}

func (h *Handler) deleteTeamSkill(w http.ResponseWriter, r *http.Request) {
	h.deleteScopedSkill(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("sid"))
}

func (h *Handler) teamSkillVersions(w http.ResponseWriter, r *http.Request) {
	h.scopedVersions(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("sid"))
}

func (h *Handler) teamSkillVersionAt(w http.ResponseWriter, r *http.Request) {
	h.scopedVersionAt(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("sid"))
}

func (h *Handler) rollbackTeamSkill(w http.ResponseWriter, r *http.Request) {
	h.rollbackScopedSkill(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("sid"))
}
