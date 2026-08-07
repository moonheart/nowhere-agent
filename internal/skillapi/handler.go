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

// RegisterAuthed mounts every route behind the auth middleware, so each handler
// can rely on an authenticated user being on the request context.
func (h *Handler) RegisterAuthed(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	// ---- self (user scope) ----
	route(mux, auth, "GET /api/me/skills", h.mySkills)
	route(mux, auth, "POST /api/me/skills", h.createMySkill)
	route(mux, auth, "GET /api/me/skills/{id}", h.getMySkill)
	route(mux, auth, "PUT /api/me/skills/{id}", h.updateMySkill)
	route(mux, auth, "DELETE /api/me/skills/{id}", h.deleteMySkill)
	route(mux, auth, "GET /api/me/skills/{id}/versions", h.mySkillVersions)
	route(mux, auth, "GET /api/me/skills/{id}/versions/{v}", h.mySkillVersionAt)
	route(mux, auth, "POST /api/me/skills/{id}/rollback/{v}", h.rollbackMySkill)
	route(mux, auth, "POST /api/me/skills/{id}/enable", h.enableMySkill)
	route(mux, auth, "POST /api/me/skills/{id}/disable", h.disableMySkill)
	route(mux, auth, "POST /api/me/skills/{id}/move", h.moveMySkill)

	// ---- team (team scope): members read, admins write ----
	route(mux, auth, "GET /api/teams/{id}/skills", h.requireTeamRole(identity.RoleMember, h.teamSkills))
	route(mux, auth, "POST /api/teams/{id}/skills", h.requireTeamRole(identity.RoleAdmin, h.createTeamSkill))
	route(mux, auth, "GET /api/teams/{id}/skills/{sid}", h.requireTeamRole(identity.RoleMember, h.getTeamSkill))
	route(mux, auth, "PUT /api/teams/{id}/skills/{sid}", h.requireTeamRole(identity.RoleAdmin, h.updateTeamSkill))
	route(mux, auth, "DELETE /api/teams/{id}/skills/{sid}", h.requireTeamRole(identity.RoleAdmin, h.deleteTeamSkill))
	route(mux, auth, "GET /api/teams/{id}/skills/{sid}/versions", h.requireTeamRole(identity.RoleMember, h.teamSkillVersions))
	route(mux, auth, "GET /api/teams/{id}/skills/{sid}/versions/{v}", h.requireTeamRole(identity.RoleMember, h.teamSkillVersionAt))
	route(mux, auth, "POST /api/teams/{id}/skills/{sid}/rollback/{v}", h.requireTeamRole(identity.RoleAdmin, h.rollbackTeamSkill))
	route(mux, auth, "POST /api/teams/{id}/skills/{sid}/enable", h.requireTeamRole(identity.RoleAdmin, h.enableTeamSkill))
	route(mux, auth, "POST /api/teams/{id}/skills/{sid}/disable", h.requireTeamRole(identity.RoleAdmin, h.disableTeamSkill))

	// ---- platform (system scope) ----
	route(mux, auth, "GET /api/admin/skills", h.requireAdmin(h.systemSkills))
	route(mux, auth, "POST /api/admin/skills", h.requireAdmin(h.createSystemSkill))
	route(mux, auth, "GET /api/admin/skills/{id}", h.requireAdmin(h.getSystemSkill))
	route(mux, auth, "PUT /api/admin/skills/{id}", h.requireAdmin(h.updateSystemSkill))
	route(mux, auth, "DELETE /api/admin/skills/{id}", h.requireAdmin(h.deleteSystemSkill))
	route(mux, auth, "GET /api/admin/skills/{id}/versions", h.requireAdmin(h.systemSkillVersions))
	route(mux, auth, "GET /api/admin/skills/{id}/versions/{v}", h.requireAdmin(h.systemSkillVersionAt))
	route(mux, auth, "POST /api/admin/skills/{id}/rollback/{v}", h.requireAdmin(h.rollbackSystemSkill))
	route(mux, auth, "POST /api/admin/skills/{id}/enable", h.requireAdmin(h.enableSystemSkill))
	route(mux, auth, "POST /api/admin/skills/{id}/disable", h.requireAdmin(h.disableSystemSkill))
}

// route mounts one pattern behind the auth middleware.
func route(mux *http.ServeMux, auth func(http.Handler) http.Handler, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, auth(h))
}
