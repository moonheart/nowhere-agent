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
