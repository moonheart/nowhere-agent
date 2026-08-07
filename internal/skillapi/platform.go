package skillapi

import (
	"net/http"

	"nowhere-agent/internal/identity"
)

// Platform-tier routes (/api/admin/skills/**), gated on platform_role == admin.
// They manage system-scope (global) skills — visible to every account.

func (h *Handler) systemSkills(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	sks, err := h.store.ListByScope(r.Context(), identity.SystemScope())
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skillDTOs(sks)})
}

func (h *Handler) createSystemSkill(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	var req skillRequest
	if !decode(w, r, &req) {
		return
	}
	sk, ok := req.toSkill(w, identity.SystemScope())
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

func (h *Handler) getSystemSkill(w http.ResponseWriter, r *http.Request) {
	h.getScopedSkill(w, r, identity.SystemScope(), r.PathValue("id"))
}

func (h *Handler) updateSystemSkill(w http.ResponseWriter, r *http.Request) {
	h.updateScopedSkill(w, r, identity.SystemScope(), r.PathValue("id"))
}

func (h *Handler) deleteSystemSkill(w http.ResponseWriter, r *http.Request) {
	h.deleteScopedSkill(w, r, identity.SystemScope(), r.PathValue("id"))
}

func (h *Handler) systemSkillVersions(w http.ResponseWriter, r *http.Request) {
	h.scopedVersions(w, r, identity.SystemScope(), r.PathValue("id"))
}

func (h *Handler) systemSkillVersionAt(w http.ResponseWriter, r *http.Request) {
	h.scopedVersionAt(w, r, identity.SystemScope(), r.PathValue("id"))
}

func (h *Handler) rollbackSystemSkill(w http.ResponseWriter, r *http.Request) {
	h.rollbackScopedSkill(w, r, identity.SystemScope(), r.PathValue("id"))
}

func (h *Handler) enableSystemSkill(w http.ResponseWriter, r *http.Request) {
	h.setScopedSkillEnabled(w, r, identity.SystemScope(), r.PathValue("id"), true)
}

func (h *Handler) disableSystemSkill(w http.ResponseWriter, r *http.Request) {
	h.setScopedSkillEnabled(w, r, identity.SystemScope(), r.PathValue("id"), false)
}
