package agentdefapi

import (
	"fmt"
	"net/http"

	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/identity"
)

// Shared scoped operations, parameterized by the owning scope. Reads and
// writes are keyed by (name, scope): a name from another scope is reported
// not-found, never read, written, or deleted. Built-in definitions are not in
// the store at all, so acting on a built-in-only name reads as not-found —
// overriding a built-in is only possible by POSTing a same-named definition
// into a scope.

func (h *Handler) listScopedDefs(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef) {
	if h.storeUnavailable(w) {
		return
	}
	defs, err := h.store.ListByScope(r.Context(), scope)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"defs": defDTOs(defs)})
}

func (h *Handler) createScopedDef(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef) {
	if h.storeUnavailable(w) {
		return
	}
	var req defRequest
	if !decode(w, r, &req) {
		return
	}
	h.saveScopedDef(w, r, scope, "", req)
}

func (h *Handler) getScopedDef(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, name string) {
	if h.storeUnavailable(w) {
		return
	}
	sd, err := h.store.Get(r.Context(), name, scope)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"def": defDTOOf(sd)})
}

func (h *Handler) updateScopedDef(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, name string) {
	if h.storeUnavailable(w) {
		return
	}
	// Resolve first: the update applies to THIS definition, so it must already
	// exist in the scope (a PUT to a foreign or missing name is not an upsert).
	if _, err := h.store.Get(r.Context(), name, scope); err != nil {
		h.writeStoreError(w, err)
		return
	}
	var req defRequest
	if !decode(w, r, &req) {
		return
	}
	h.saveScopedDef(w, r, scope, name, req)
}

// saveScopedDef validates and stores the document. On update (wantName set)
// the document's frontmatter name must match the path name — renaming is a
// delete+create, not an edit.
func (h *Handler) saveScopedDef(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, wantName string, req defRequest) {
	d, err := agentdef.Validate(req.Document)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid agent document: "+err.Error())
		return
	}
	if wantName != "" && d.Name != wantName {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("document name %q does not match path name %q", d.Name, wantName))
		return
	}
	saved, err := h.store.Put(r.Context(), req.Document, scope, caller(r).ID)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"def":      defDTOOf(saved),
		"warnings": h.warnings(r, saved.AgentDef, scope),
	})
}

func (h *Handler) deleteScopedDef(w http.ResponseWriter, r *http.Request, scope identity.ScopeRef, name string) {
	if h.storeUnavailable(w) {
		return
	}
	if err := h.store.Delete(r.Context(), name, scope); err != nil {
		h.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// warnings flags a definition whose declared skills cannot run right now (no
// script runner for the caller's scopes — the same degradation the spawn path
// logs). Nil check disabled → no warnings.
func (h *Handler) warnings(r *http.Request, d agentdef.AgentDef, scope identity.ScopeRef) []string {
	if h.skillsRunnable == nil || len(d.Skills) == 0 {
		return nil
	}
	if h.skillsRunnable(r.Context(), []identity.ScopeRef{scope}) {
		return nil
	}
	return []string{"this definition declares skills, but no skill script runner is available (exec disabled or no visible skill has scripts); the skills are currently ineffective"}
}
