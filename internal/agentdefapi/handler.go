// Package agentdefapi is the HTTP surface of agent-definition management
// (change persist-agent-defs). It contains no SQL: definitions come from the
// agentdef PGStore. Its own job is routing, authorization, and DTOs.
//
// Routes fall into the same three tiers as the skill console, visible in the
// path:
//
//	/api/me/agentdefs        self     — any authenticated account, its own defs
//	/api/teams/{id}/agentdefs team    — members read, team admins write
//	/api/admin/agentdefs     platform — platform_role == admin (system scope)
package agentdefapi

import (
	"context"

	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
)

// Store is the persistence surface the handler needs. *agentdef.PGStore
// satisfies it.
type Store interface {
	ListByScope(ctx context.Context, scope identity.ScopeRef) ([]agentdef.StoredDef, error)
	Get(ctx context.Context, name string, scope identity.ScopeRef) (agentdef.StoredDef, error)
	Put(ctx context.Context, document string, scope identity.ScopeRef, createdBy string) (agentdef.StoredDef, error)
	Delete(ctx context.Context, name string, scope identity.ScopeRef) error
}

// SkillsRunnable reports whether skill scripts can actually execute for the
// given scopes right now (exec enabled and some visible skill has scripts) —
// the same condition the spawn path checks. A written definition declaring
// `skills` while this is false gets a warning on the write response. Nil
// disables the check.
type SkillsRunnable func(ctx context.Context, scopes []identity.ScopeRef) bool

// Handler serves the agent-definition management endpoints.
type Handler struct {
	identity       *identity.Service
	store          Store
	skillsRunnable SkillsRunnable
}

// NewHandler builds the handler. store may be nil; the routes then answer 503
// rather than panicking, keeping a deployment without a database serving the
// rest.
func NewHandler(id *identity.Service, store Store, runnable SkillsRunnable) *Handler {
	return &Handler{identity: id, store: store, skillsRunnable: runnable}
}

// RegisterAuthed mounts every route onto the protected group. Auth is NOT
// wrapped per route: the group applies its middleware set once at Mount time,
// so this handler only declares which routes belong to the protected tier.
func (h *Handler) RegisterAuthed(g *httpx.Router) {
	// ---- self (user scope) ----
	g.HandleFunc("GET /api/me/agentdefs", h.myDefs)
	g.HandleFunc("POST /api/me/agentdefs", h.createMyDef)
	g.HandleFunc("GET /api/me/agentdefs/{name}", h.getMyDef)
	g.HandleFunc("PUT /api/me/agentdefs/{name}", h.updateMyDef)
	g.HandleFunc("DELETE /api/me/agentdefs/{name}", h.deleteMyDef)

	// ---- team (team scope): members read, admins write ----
	g.HandleFunc("GET /api/teams/{id}/agentdefs", h.requireTeamRole(identity.RoleMember, h.teamDefs))
	g.HandleFunc("POST /api/teams/{id}/agentdefs", h.requireTeamRole(identity.RoleAdmin, h.createTeamDef))
	g.HandleFunc("GET /api/teams/{id}/agentdefs/{name}", h.requireTeamRole(identity.RoleMember, h.getTeamDef))
	g.HandleFunc("PUT /api/teams/{id}/agentdefs/{name}", h.requireTeamRole(identity.RoleAdmin, h.updateTeamDef))
	g.HandleFunc("DELETE /api/teams/{id}/agentdefs/{name}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamDef))

	// ---- platform (system scope) ----
	g.HandleFunc("GET /api/admin/agentdefs", h.requireAdmin(h.systemDefs))
	g.HandleFunc("POST /api/admin/agentdefs", h.requireAdmin(h.createSystemDef))
	g.HandleFunc("GET /api/admin/agentdefs/{name}", h.requireAdmin(h.getSystemDef))
	g.HandleFunc("PUT /api/admin/agentdefs/{name}", h.requireAdmin(h.updateSystemDef))
	g.HandleFunc("DELETE /api/admin/agentdefs/{name}", h.requireAdmin(h.deleteSystemDef))
}
