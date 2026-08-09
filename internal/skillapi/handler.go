// Package skillapi is the HTTP surface of skill management (skill-console). It
// contains no SQL: skills come from the skill store. Its own job is routing,
// authorization, and DTOs.
//
// Routes fall into the same three tiers as the admin console, visible in the path:
//
//	/api/me/skills      self     — any authenticated account, its own user skills
//	/api/teams/{id}/skills team  — authorized by the caller's role IN THAT TEAM
//	/api/admin/skills   platform — authorized by platform_role == admin (system scope)
package skillapi

import (
	"context"
	"net/http"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/skill"
)

// Store is the persistence surface the handler needs. *skill.PGStore satisfies it.
type Store interface {
	ByID(ctx context.Context, id string) (skill.Skill, error)
	ListByScope(ctx context.Context, scope identity.ScopeRef) ([]skill.Skill, error)
	Put(ctx context.Context, sk skill.Skill, createdBy string) (skill.Skill, error)
	Versions(ctx context.Context, skillID string) ([]skill.Version, error)
	VersionAt(ctx context.Context, skillID string, version int) (skill.Skill, error)
	Rollback(ctx context.Context, skillID string, version int, createdBy string) (skill.Skill, error)
	Delete(ctx context.Context, id string) error
	SetEnabled(ctx context.Context, id string, enabled bool) (skill.Skill, error)
	MoveToTeam(ctx context.Context, id, teamID string) (skill.Skill, error)
}

// Handler serves the skill-management endpoints.
type Handler struct {
	identity *identity.Service
	store    Store
}

// NewHandler builds the skill handler. store may be nil; the routes then answer
// 503 rather than panicking, keeping a deployment without a database serving
// the rest.
func NewHandler(id *identity.Service, store Store) *Handler {
	return &Handler{identity: id, store: store}
}

// RegisterAuthed mounts every route onto the protected group. Auth is NOT
// wrapped per route: the group applies its middleware set once at Mount time,
// so this handler only declares which routes belong to the protected tier. Each
// handler relies on an authenticated user being on the request context.
func (h *Handler) RegisterAuthed(g *httpx.Router) {
	// ---- self (user scope) ----
	route(g, "GET /api/me/skills", h.mySkills)
	route(g, "POST /api/me/skills", h.createMySkill)
	route(g, "GET /api/me/skills/{id}", h.getMySkill)
	route(g, "PUT /api/me/skills/{id}", h.updateMySkill)
	route(g, "DELETE /api/me/skills/{id}", h.deleteMySkill)
	route(g, "GET /api/me/skills/{id}/versions", h.mySkillVersions)
	route(g, "GET /api/me/skills/{id}/versions/{v}", h.mySkillVersionAt)
	route(g, "POST /api/me/skills/{id}/rollback/{v}", h.rollbackMySkill)
	route(g, "POST /api/me/skills/{id}/enable", h.enableMySkill)
	route(g, "POST /api/me/skills/{id}/disable", h.disableMySkill)
	route(g, "POST /api/me/skills/{id}/move", h.moveMySkill)

	// ---- team (team scope): members read, admins write ----
	route(g, "GET /api/teams/{id}/skills", h.requireTeamRole(identity.RoleMember, h.teamSkills))
	route(g, "POST /api/teams/{id}/skills", h.requireTeamRole(identity.RoleAdmin, h.createTeamSkill))
	route(g, "GET /api/teams/{id}/skills/{sid}", h.requireTeamRole(identity.RoleMember, h.getTeamSkill))
	route(g, "PUT /api/teams/{id}/skills/{sid}", h.requireTeamRole(identity.RoleAdmin, h.updateTeamSkill))
	route(g, "DELETE /api/teams/{id}/skills/{sid}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamSkill))
	route(g, "GET /api/teams/{id}/skills/{sid}/versions", h.requireTeamRole(identity.RoleMember, h.teamSkillVersions))
	route(g, "GET /api/teams/{id}/skills/{sid}/versions/{v}", h.requireTeamRole(identity.RoleMember, h.teamSkillVersionAt))
	route(g, "POST /api/teams/{id}/skills/{sid}/rollback/{v}", h.requireTeamRole(identity.RoleAdmin, h.rollbackTeamSkill))
	route(g, "POST /api/teams/{id}/skills/{sid}/enable", h.requireTeamRole(identity.RoleAdmin, h.enableTeamSkill))
	route(g, "POST /api/teams/{id}/skills/{sid}/disable", h.requireTeamRole(identity.RoleAdmin, h.disableTeamSkill))

	// ---- platform (system scope) ----
	route(g, "GET /api/admin/skills", h.requireAdmin(h.systemSkills))
	route(g, "POST /api/admin/skills", h.requireAdmin(h.createSystemSkill))
	route(g, "GET /api/admin/skills/{id}", h.requireAdmin(h.getSystemSkill))
	route(g, "PUT /api/admin/skills/{id}", h.requireAdmin(h.updateSystemSkill))
	route(g, "DELETE /api/admin/skills/{id}", h.requireAdmin(h.deleteSystemSkill))
	route(g, "GET /api/admin/skills/{id}/versions", h.requireAdmin(h.systemSkillVersions))
	route(g, "GET /api/admin/skills/{id}/versions/{v}", h.requireAdmin(h.systemSkillVersionAt))
	route(g, "POST /api/admin/skills/{id}/rollback/{v}", h.requireAdmin(h.rollbackSystemSkill))
	route(g, "POST /api/admin/skills/{id}/enable", h.requireAdmin(h.enableSystemSkill))
	route(g, "POST /api/admin/skills/{id}/disable", h.requireAdmin(h.disableSystemSkill))
}

// route registers one pattern onto the protected group.
func route(g *httpx.Router, pattern string, h http.HandlerFunc) {
	g.HandleFunc(pattern, h)
}
