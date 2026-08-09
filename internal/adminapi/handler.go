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

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/routing"
	"nowhere-agent/internal/usage"
)

// Handler serves the console's endpoints.
type Handler struct {
	identity *identity.Service
	keys     *routing.PGKeyStore
	usage    *usage.Store
	quotas   *quota.Store
	memories memory.Port
	dreaming *dreaming.Runner
	// audit records administrative actions; nil disables recording.
	audit *audit.Logger
}

// NewHandler builds the console handler. keys, usage, and memories may be nil;
// the routes that need them then answer 503 rather than panicking, which keeps
// a deployment without a memory port or provider keys serving the rest.
func NewHandler(id *identity.Service, keys *routing.PGKeyStore, u *usage.Store, mem memory.Port) *Handler {
	return &Handler{identity: id, keys: keys, usage: u, memories: mem}
}

// WithQuotas wires the usage-budget store, enabling the quota configuration
// routes (enterprise-readiness P1-1 management face). Left nil, /api/admin/quotas
// answers 503 — the console still serves everything else.
func (h *Handler) WithQuotas(q *quota.Store) *Handler {
	h.quotas = q
	return h
}

// WithAudit wires the audit trail so administrative actions are recorded.
// Recording is best-effort and never changes a response (see record).
func (h *Handler) WithAudit(l *audit.Logger) *Handler {
	h.audit = l
	return h
}

// WithDreaming wires the consolidation runner, enabling the manual trigger.
// Left nil, /api/me/dream answers 503 — the console still serves everything
// else, which matters because the runner needs a configured model and the rest
// of the console does not.
func (h *Handler) WithDreaming(r *dreaming.Runner) *Handler {
	h.dreaming = r
	return h
}

// RegisterAuthed mounts every console route onto the protected group. Auth is
// NOT wrapped per route: the group applies its middleware set once at Mount
// time, so this handler only declares which routes belong to the protected
// tier. Each handler relies on an authenticated user being on the request
// context.
//
// Ordering: auth runs outermost (group middleware), then this package's tier
// guard. Registering the guard outside auth would have it read an empty user
// and reject everyone.
func (h *Handler) RegisterAuthed(g *httpx.Router) {
	// ---- self ----
	route(g, "PATCH /api/me", h.updateMe)
	route(g, "POST /api/me/password", h.changePassword)
	route(g, "GET /api/me/usage", h.myUsage)
	route(g, "GET /api/me/memories", h.myMemories)
	route(g, "DELETE /api/me/memories/{id}", h.deleteMyMemory)
	route(g, "GET /api/me/dream", h.dreamStatus)
	route(g, "POST /api/me/dream", h.triggerDream)
	route(g, "GET /api/me/tokens", h.myTokens)
	route(g, "DELETE /api/me/tokens", h.revokeOtherTokens)
	route(g, "DELETE /api/me/tokens/{id}", h.revokeToken)

	// ---- teams ----
	route(g, "GET /api/teams", h.myTeams)
	route(g, "POST /api/teams", h.createTeam)
	route(g, "GET /api/teams/{id}", h.requireTeamRole(identity.RoleMember, h.getTeam))
	route(g, "PATCH /api/teams/{id}", h.requireTeamRole(identity.RoleAdmin, h.renameTeam))
	route(g, "DELETE /api/teams/{id}", h.requireTeamRole(identity.RoleOwner, h.deleteTeam))

	route(g, "GET /api/teams/{id}/members", h.requireTeamRole(identity.RoleMember, h.listMembers))
	route(g, "POST /api/teams/{id}/members", h.requireTeamRole(identity.RoleAdmin, h.addMember))
	route(g, "PATCH /api/teams/{id}/members/{userId}", h.requireTeamRole(identity.RoleOwner, h.changeMemberRole))
	// Removal is the one team route a plain member may reach, because leaving
	// is removing yourself. The handler distinguishes the two cases.
	route(g, "DELETE /api/teams/{id}/members/{userId}", h.requireTeamRole(identity.RoleMember, h.removeMember))

	route(g, "GET /api/teams/{id}/keys", h.requireTeamRole(identity.RoleAdmin, h.listKeys))
	route(g, "PUT /api/teams/{id}/keys/{provider}", h.requireTeamRole(identity.RoleAdmin, h.putKey))
	route(g, "DELETE /api/teams/{id}/keys/{provider}", h.requireTeamRole(identity.RoleAdmin, h.deleteKey))

	route(g, "GET /api/teams/{id}/usage", h.requireTeamRole(identity.RoleAdmin, h.teamUsage))
	route(g, "GET /api/teams/{id}/memories", h.requireTeamRole(identity.RoleMember, h.teamMemories))
	route(g, "DELETE /api/teams/{id}/memories/{mid}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamMemory))
	route(g, "POST /api/teams/{id}/memories/{mid}/deprecate", h.requireTeamRole(identity.RoleAdmin, h.deprecateTeamMemory))

	// ---- platform ----
	route(g, "GET /api/admin/stats", h.requireAdmin(h.stats))
	route(g, "GET /api/admin/users", h.requireAdmin(h.listUsers))
	route(g, "POST /api/admin/users", h.requireAdmin(h.createUser))
	route(g, "PATCH /api/admin/users/{id}", h.requireAdmin(h.patchUser))
	route(g, "POST /api/admin/users/{id}/password", h.requireAdmin(h.resetPassword))
	route(g, "DELETE /api/admin/users/{id}", h.requireAdmin(h.deleteUser))
	route(g, "GET /api/admin/teams", h.requireAdmin(h.listAllTeams))
	route(g, "POST /api/admin/teams", h.requireAdmin(h.createTeamForOwner))
	route(g, "GET /api/admin/usage", h.requireAdmin(h.platformUsage))
	route(g, "GET /api/admin/quotas", h.requireAdmin(h.listQuotas))
	route(g, "PUT /api/admin/quotas", h.requireAdmin(h.putQuota))
	route(g, "DELETE /api/admin/quotas", h.requireAdmin(h.clearQuota))
	route(g, "GET /api/admin/memories", h.requireAdmin(h.adminMemories))
	route(g, "DELETE /api/admin/memories/{id}", h.requireAdmin(h.adminDeleteMemory))
	route(g, "POST /api/admin/memories/{id}/deprecate", h.requireAdmin(h.adminDeprecateMemory))
	route(g, "GET /api/admin/audit", h.requireAdmin(h.listAudit))
}

// route registers one pattern onto the protected group.
func route(g *httpx.Router, pattern string, h http.HandlerFunc) {
	g.HandleFunc(pattern, h)
}

// record writes one event to the audit trail when one is wired, attributing it
// to the request's authenticated caller. It is a no-op when no trail is wired,
// and never affects the response — LogAndReport swallows the error, so a broken
// audit sink cannot turn a successful admin action into a failed one.
func (h *Handler) record(r *http.Request, e audit.Event) {
	if h.audit == nil {
		return
	}
	u := caller(r)
	h.audit.LogAndReport(r.Context(), e.FromRequest(r).Actor(u.ID, u.Email))
}
