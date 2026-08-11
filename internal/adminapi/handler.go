// Package adminapi is the HTTP surface of the management console
// (admin-console). It contains no SQL: users, teams, memberships, and tokens
// come from identity; token accounting from usage; memories from the memory
// port. Its own job is routing, authorization, and DTOs.
//
// Routes fall into three tiers, and the tier is visible in the path:
//
//	/api/me/**      self    — any authenticated account, its own resources
//	/api/teams/**   team    — authorized by the caller's role IN THAT TEAM
//	/api/admin/**   platform— authorized by platform_role == admin
package adminapi

import (
	"context"
	"net/http"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/export"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/usage"
	"nowhere-agent/internal/webhook"
)

// Handler serves the console's endpoints.
type Handler struct {
	identity *identity.Service
	usage    *usage.Store
	quotas   *quota.Store
	memories memory.Port
	dreaming *dreaming.Runner
	// providers is the provider registry (system CRUD + team assignment). Nil
	// disables the provider routes with a 503 (see WithProviders).
	providers providerreg.Store
	// uploads is the user-level image upload service. Nil disables the
	// /api/me/uploads routes with a 503 (see WithUploads).
	uploads upload.Uploader
	// audit records administrative actions; nil disables recording.
	audit *audit.Logger
	// exporter assembles a user's data footprint for /api/me/export. Nil
	// disables the route with a 503 (see WithExporter).
	exporter *export.Service
	// deliveries exposes the persistent webhook outbox (delivery history +
	// manual requeue) to platform admins. Nil disables the routes (503).
	deliveries DeliveryStore
}

// DeliveryStore is the outbox surface the admin routes need. *webhook.
// DeliveryStore satisfies it; an interface keeps the console free of a
// webhook dependency.
type DeliveryStore interface {
	List(ctx context.Context, status string, limit, offset int) ([]webhook.Delivery, int, error)
	Requeue(ctx context.Context, id string) error
}

// NewHandler builds the console handler. usage and memories may be nil; the
// routes that need them then answer 503 rather than panicking, which keeps a
// deployment without a memory port serving the rest.
func NewHandler(id *identity.Service, u *usage.Store, mem memory.Port) *Handler {
	return &Handler{identity: id, usage: u, memories: mem}
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

// WithWebhookDeliveries wires the persistent webhook outbox admin routes
// (delivery history + manual requeue). Left nil, the routes answer 503.
func (h *Handler) WithWebhookDeliveries(s DeliveryStore) *Handler {
	h.deliveries = s
	return h
}

// WithExporter wires the user data-export service, enabling GET /api/me/export.
// Left nil, the route answers 503 — the console still serves everything else.
func (h *Handler) WithExporter(e *export.Service) *Handler {
	h.exporter = e
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

// WithProviders wires the provider registry, enabling the provider
// administration routes (system CRUD + team provider/assignment). Left nil,
// those routes answer 503 — the rest of the console still serves.
func (h *Handler) WithProviders(s providerreg.Store) *Handler {
	h.providers = s
	return h
}

// WithUploads wires the user-level image upload service, enabling the
// /api/me/uploads list + delete routes. Left nil, those answer 503.
func (h *Handler) WithUploads(u upload.Uploader) *Handler {
	h.uploads = u
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
	route(g, "DELETE /api/me", h.deleteMe)
	route(g, "POST /api/me/password", h.changePassword)
	route(g, "POST /api/me/totp/enable", h.enableTOTP)
	route(g, "POST /api/me/totp/confirm", h.confirmTOTP)
	route(g, "POST /api/me/totp/disable", h.disableTOTP)
	route(g, "GET /api/me/usage", h.myUsage)
	route(g, "GET /api/me/memories", h.myMemories)
	route(g, "DELETE /api/me/memories/{id}", h.deleteMyMemory)
	route(g, "GET /api/me/dream", h.dreamStatus)
	route(g, "POST /api/me/dream", h.triggerDream)
	route(g, "GET /api/me/tokens", h.myTokens)
	route(g, "DELETE /api/me/tokens", h.revokeOtherTokens)
	route(g, "DELETE /api/me/tokens/{id}", h.revokeToken)
	route(g, "GET /api/me/uploads", h.listUploads)
	route(g, "DELETE /api/me/uploads/{id}", h.deleteUpload)
	route(g, "GET /api/me/export", h.exportData)

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

	route(g, "GET /api/teams/{id}/usage", h.requireTeamRole(identity.RoleAdmin, h.teamUsage))
	route(g, "GET /api/teams/{id}/memories", h.requireTeamRole(identity.RoleMember, h.teamMemories))
	route(g, "DELETE /api/teams/{id}/memories/{mid}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamMemory))
	route(g, "POST /api/teams/{id}/memories/{mid}/deprecate", h.requireTeamRole(identity.RoleAdmin, h.deprecateTeamMemory))

	// Provider registry (change provider-registry): teams see the providers
	// available to them (system + their own) and manage their own providers,
	// their models, and their provider assignment.
	route(g, "GET /api/teams/{id}/providers", h.requireTeamRole(identity.RoleMember, h.listTeamProviders))
	route(g, "POST /api/teams/{id}/providers", h.requireTeamRole(identity.RoleAdmin, h.createTeamProvider))
	route(g, "PATCH /api/teams/{id}/providers/{pid}", h.requireTeamRole(identity.RoleAdmin, h.updateTeamProvider))
	route(g, "DELETE /api/teams/{id}/providers/{pid}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamProvider))
	route(g, "GET /api/teams/{id}/providers/{pid}/models", h.requireTeamRole(identity.RoleMember, h.listProviderModels))
	route(g, "POST /api/teams/{id}/providers/{pid}/models/fetch", h.requireTeamRole(identity.RoleAdmin, h.fetchTeamModels))
	route(g, "POST /api/teams/{id}/providers/{pid}/models", h.requireTeamRole(identity.RoleAdmin, h.createTeamModel))
	route(g, "PATCH /api/teams/{id}/providers/{pid}/models/{mid}", h.requireTeamRole(identity.RoleAdmin, h.updateTeamModel))
	route(g, "DELETE /api/teams/{id}/providers/{pid}/models/{mid}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamModel))
	route(g, "POST /api/teams/{id}/providers/{pid}/models/{mid}/default", h.requireTeamRole(identity.RoleAdmin, h.setTeamDefaultModel))
	route(g, "PUT /api/teams/{id}/provider-assignment", h.requireTeamRole(identity.RoleAdmin, h.setTeamAssignment))
	route(g, "DELETE /api/teams/{id}/provider-assignment", h.requireTeamRole(identity.RoleAdmin, h.clearTeamAssignment))

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
	route(g, "GET /api/admin/service-keys", h.requireAdmin(h.listServiceKeys))
	route(g, "POST /api/admin/service-keys", h.requireAdmin(h.createServiceKey))
	route(g, "DELETE /api/admin/service-keys/{id}", h.requireAdmin(h.revokeServiceKey))

	route(g, "GET /api/admin/webhook-deliveries", h.requireAdmin(h.listDeliveries))
	route(g, "POST /api/admin/webhook-deliveries/{id}/retry", h.requireAdmin(h.requeueDelivery))

	// Provider registry, platform tier (change provider-registry): system
	// providers and their models are platform-managed; one of them is the
	// platform default every team falls back to.
	route(g, "GET /api/admin/providers", h.requireAdmin(h.listSystemProviders))
	route(g, "POST /api/admin/providers", h.requireAdmin(h.createSystemProvider))
	route(g, "PATCH /api/admin/providers/{pid}", h.requireAdmin(h.updateSystemProvider))
	route(g, "DELETE /api/admin/providers/{pid}", h.requireAdmin(h.deleteSystemProvider))
	route(g, "POST /api/admin/providers/{pid}/default", h.requireAdmin(h.setSystemDefaultProvider))
	route(g, "GET /api/admin/providers/{pid}/models", h.requireAdmin(h.listProviderModels))
	route(g, "POST /api/admin/providers/{pid}/models/fetch", h.requireAdmin(h.fetchSystemModels))
	route(g, "POST /api/admin/providers/{pid}/models", h.requireAdmin(h.createSystemModel))
	route(g, "PATCH /api/admin/providers/{pid}/models/{mid}", h.requireAdmin(h.updateSystemModel))
	route(g, "DELETE /api/admin/providers/{pid}/models/{mid}", h.requireAdmin(h.deleteSystemModel))
	route(g, "POST /api/admin/providers/{pid}/models/{mid}/default", h.requireAdmin(h.setSystemDefaultModel))
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
