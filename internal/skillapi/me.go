package skillapi

import (
	"net/http"

	"nowhere-agent/internal/identity"
)

// Self-service routes (/api/me/skills/**). They need no tier guard beyond
// authentication: each resolves its scope from the authenticated caller's own
// user scope, so there is nothing further to authorize on create/list. Single
// skill operations still verify the resolved skill sits in that scope.

func (h *Handler) mySkills(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	u := caller(r)
	sks, err := h.store.ListByScope(r.Context(), identity.UserScope(u.ID))
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skillDTOs(sks)})
}

func (h *Handler) createMySkill(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	var req skillRequest
	if !decode(w, r, &req) {
		return
	}
	u := caller(r)
	sk, ok := req.toSkill(w, identity.UserScope(u.ID))
	if !ok {
		return
	}
	saved, err := h.store.Put(r.Context(), sk, u.ID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"skill": skillDTOOf(saved)})
}

func (h *Handler) getMySkill(w http.ResponseWriter, r *http.Request) {
	h.getScopedSkill(w, r, identity.UserScope(caller(r).ID), r.PathValue("id"))
}

func (h *Handler) updateMySkill(w http.ResponseWriter, r *http.Request) {
	h.updateScopedSkill(w, r, identity.UserScope(caller(r).ID), r.PathValue("id"))
}

func (h *Handler) deleteMySkill(w http.ResponseWriter, r *http.Request) {
	h.deleteScopedSkill(w, r, identity.UserScope(caller(r).ID), r.PathValue("id"))
}

func (h *Handler) mySkillVersions(w http.ResponseWriter, r *http.Request) {
	h.scopedVersions(w, r, identity.UserScope(caller(r).ID), r.PathValue("id"))
}

func (h *Handler) mySkillVersionAt(w http.ResponseWriter, r *http.Request) {
	h.scopedVersionAt(w, r, identity.UserScope(caller(r).ID), r.PathValue("id"))
}

func (h *Handler) rollbackMySkill(w http.ResponseWriter, r *http.Request) {
	h.rollbackScopedSkill(w, r, identity.UserScope(caller(r).ID), r.PathValue("id"))
}
