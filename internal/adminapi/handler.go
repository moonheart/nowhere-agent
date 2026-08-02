// Package adminapi is the HTTP surface of the management console
// (admin-console). It contains no SQL: users, teams, memberships, and tokens
// come from identity; team provider keys from routing; token accounting from
// usage; memories from the memory port. Its own job is routing, authorization,
// and DTOs.
//
// Routes fall into three tiers, and the tier is visible in the path:
//
//	/api/me/**      self    — any authenticated account, its own resources
//	/api/teams/**   team    — authorized by the caller's role IN THAT TEAM
//	/api/admin/**   platform— authorized by platform_role == admin
package adminapi

import (
	"net/http"

	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/routing"
	"nowhere-agent/internal/usage"
)

// Handler serves the console's endpoints.
type Handler struct {
	identity *identity.Service
	keys     *routing.PGKeyStore
	usage    *usage.Store
	memories memory.Port
	dreaming *dreaming.Runner
}

// NewHandler builds the console handler. keys, usage, and memories may be nil;
// the routes that need them then answer 503 rather than panicking, which keeps
// a deployment without a memory port or provider keys serving the rest.
func NewHandler(id *identity.Service, keys *routing.PGKeyStore, u *usage.Store, mem memory.Port) *Handler {
	return &Handler{identity: id, keys: keys, usage: u, memories: mem}
}

// WithDreaming wires the consolidation runner, enabling the manual trigger.
// Left nil, /api/me/dream answers 503 — the console still serves everything
// else, which matters because the runner needs a configured model and the rest
// of the console does not.
func (h *Handler) WithDreaming(r *dreaming.Runner) *Handler {
	h.dreaming = r
	return h
}

// RegisterAuthed mounts every console route behind the auth middleware, so each
// handler can rely on an authenticated user being on the request context.
//
// Ordering: auth runs outermost, then this package's tier guard. Registering
// the guard outside auth would have it read an empty user and reject everyone.
func (h *Handler) RegisterAuthed(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	// ---- self ----
	route(mux, auth, "PATCH /api/me", h.updateMe)
	route(mux, auth, "POST /api/me/password", h.changePassword)
	route(mux, auth, "GET /api/me/usage", h.myUsage)
	route(mux, auth, "GET /api/me/memories", h.myMemories)
	route(mux, auth, "DELETE /api/me/memories/{id}", h.deleteMyMemory)
	route(mux, auth, "GET /api/me/dream", h.dreamStatus)
	route(mux, auth, "POST /api/me/dream", h.triggerDream)
	route(mux, auth, "GET /api/me/tokens", h.myTokens)
	route(mux, auth, "DELETE /api/me/tokens", h.revokeOtherTokens)
	route(mux, auth, "DELETE /api/me/tokens/{id}", h.revokeToken)

	// ---- teams ----
	route(mux, auth, "GET /api/teams", h.myTeams)
	route(mux, auth, "POST /api/teams", h.createTeam)
	route(mux, auth, "GET /api/teams/{id}", h.requireTeamRole(identity.RoleMember, h.getTeam))
	route(mux, auth, "PATCH /api/teams/{id}", h.requireTeamRole(identity.RoleAdmin, h.renameTeam))
	route(mux, auth, "DELETE /api/teams/{id}", h.requireTeamRole(identity.RoleOwner, h.deleteTeam))

	route(mux, auth, "GET /api/teams/{id}/members", h.requireTeamRole(identity.RoleMember, h.listMembers))
	route(mux, auth, "POST /api/teams/{id}/members", h.requireTeamRole(identity.RoleAdmin, h.addMember))
	route(mux, auth, "PATCH /api/teams/{id}/members/{userId}", h.requireTeamRole(identity.RoleOwner, h.changeMemberRole))
	// Removal is the one team route a plain member may reach, because leaving
	// is removing yourself. The handler distinguishes the two cases.
	route(mux, auth, "DELETE /api/teams/{id}/members/{userId}", h.requireTeamRole(identity.RoleMember, h.removeMember))

	route(mux, auth, "GET /api/teams/{id}/keys", h.requireTeamRole(identity.RoleAdmin, h.listKeys))
	route(mux, auth, "PUT /api/teams/{id}/keys/{provider}", h.requireTeamRole(identity.RoleAdmin, h.putKey))
	route(mux, auth, "DELETE /api/teams/{id}/keys/{provider}", h.requireTeamRole(identity.RoleAdmin, h.deleteKey))

	route(mux, auth, "GET /api/teams/{id}/usage", h.requireTeamRole(identity.RoleAdmin, h.teamUsage))
	route(mux, auth, "GET /api/teams/{id}/memories", h.requireTeamRole(identity.RoleMember, h.teamMemories))
	route(mux, auth, "DELETE /api/teams/{id}/memories/{mid}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamMemory))
	route(mux, auth, "POST /api/teams/{id}/memories/{mid}/deprecate", h.requireTeamRole(identity.RoleAdmin, h.deprecateTeamMemory))

	// ---- platform ----
	route(mux, auth, "GET /api/admin/stats", h.requireAdmin(h.stats))
	route(mux, auth, "GET /api/admin/users", h.requireAdmin(h.listUsers))
	route(mux, auth, "POST /api/admin/users", h.requireAdmin(h.createUser))
	route(mux, auth, "PATCH /api/admin/users/{id}", h.requireAdmin(h.patchUser))
	route(mux, auth, "POST /api/admin/users/{id}/password", h.requireAdmin(h.resetPassword))
	route(mux, auth, "DELETE /api/admin/users/{id}", h.requireAdmin(h.deleteUser))
	route(mux, auth, "GET /api/admin/teams", h.requireAdmin(h.listAllTeams))
	route(mux, auth, "POST /api/admin/teams", h.requireAdmin(h.createTeamForOwner))
	route(mux, auth, "GET /api/admin/usage", h.requireAdmin(h.platformUsage))
	route(mux, auth, "GET /api/admin/memories", h.requireAdmin(h.adminMemories))
	route(mux, auth, "DELETE /api/admin/memories/{id}", h.requireAdmin(h.adminDeleteMemory))
	route(mux, auth, "POST /api/admin/memories/{id}/deprecate", h.requireAdmin(h.adminDeprecateMemory))
}

// route mounts one pattern behind the auth middleware.
func route(mux *http.ServeMux, auth func(http.Handler) http.Handler, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, auth(h))
}
