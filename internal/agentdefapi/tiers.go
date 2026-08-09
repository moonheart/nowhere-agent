package agentdefapi

import (
	"net/http"

	"nowhere-agent/internal/identity"
)

// ---- self tier (user scope) ----

func (h *Handler) myDefs(w http.ResponseWriter, r *http.Request) {
	h.listScopedDefs(w, r, identity.UserScope(caller(r).ID))
}

func (h *Handler) createMyDef(w http.ResponseWriter, r *http.Request) {
	h.createScopedDef(w, r, identity.UserScope(caller(r).ID))
}

func (h *Handler) getMyDef(w http.ResponseWriter, r *http.Request) {
	h.getScopedDef(w, r, identity.UserScope(caller(r).ID), r.PathValue("name"))
}

func (h *Handler) updateMyDef(w http.ResponseWriter, r *http.Request) {
	h.updateScopedDef(w, r, identity.UserScope(caller(r).ID), r.PathValue("name"))
}

func (h *Handler) deleteMyDef(w http.ResponseWriter, r *http.Request) {
	h.deleteScopedDef(w, r, identity.UserScope(caller(r).ID), r.PathValue("name"))
}

// ---- team tier (team scope) ----

func (h *Handler) teamDefs(w http.ResponseWriter, r *http.Request) {
	h.listScopedDefs(w, r, identity.TeamScope(r.PathValue("id")))
}

func (h *Handler) createTeamDef(w http.ResponseWriter, r *http.Request) {
	h.createScopedDef(w, r, identity.TeamScope(r.PathValue("id")))
}

func (h *Handler) getTeamDef(w http.ResponseWriter, r *http.Request) {
	h.getScopedDef(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("name"))
}

func (h *Handler) updateTeamDef(w http.ResponseWriter, r *http.Request) {
	h.updateScopedDef(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("name"))
}

func (h *Handler) deleteTeamDef(w http.ResponseWriter, r *http.Request) {
	h.deleteScopedDef(w, r, identity.TeamScope(r.PathValue("id")), r.PathValue("name"))
}

// ---- platform tier (system scope) ----

func (h *Handler) systemDefs(w http.ResponseWriter, r *http.Request) {
	h.listScopedDefs(w, r, identity.SystemScope())
}

func (h *Handler) createSystemDef(w http.ResponseWriter, r *http.Request) {
	h.createScopedDef(w, r, identity.SystemScope())
}

func (h *Handler) getSystemDef(w http.ResponseWriter, r *http.Request) {
	h.getScopedDef(w, r, identity.SystemScope(), r.PathValue("name"))
}

func (h *Handler) updateSystemDef(w http.ResponseWriter, r *http.Request) {
	h.updateScopedDef(w, r, identity.SystemScope(), r.PathValue("name"))
}

func (h *Handler) deleteSystemDef(w http.ResponseWriter, r *http.Request) {
	h.deleteScopedDef(w, r, identity.SystemScope(), r.PathValue("name"))
}
