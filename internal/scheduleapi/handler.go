// Package scheduleapi is the HTTP surface of scheduled-task management
// (scheduled-tasks capability). It contains no SQL beyond what the schedule
// store already does; its own job is routing, authorization, and DTOs.
//
// Scheduled tasks are owner-scoped: a caller manages only their own tasks, so
// the routes live under /api/me/scheduled-tasks and confine every operation to
// the authenticated caller's tasks. A task id that belongs to someone else
// reads as not-found, never read, written, or fired.
package scheduleapi

import (
	"context"
	"net/http"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/schedule"
)

// Store is the persistence surface the handler needs. *schedule.PGStore
// satisfies it.
type Store interface {
	Create(ctx context.Context, t schedule.Task) (schedule.Task, error)
	Get(ctx context.Context, id string) (schedule.Task, error)
	Update(ctx context.Context, t schedule.Task) (schedule.Task, error)
	Delete(ctx context.Context, id string) error
	SetEnabled(ctx context.Context, id string, enabled bool) error
	ListForUser(ctx context.Context, userID string) ([]schedule.Task, error)
	ListSessions(ctx context.Context, taskID string) ([]string, error)
	EndSessions(ctx context.Context, taskID string) (int, error)
}

// Handler serves the scheduled-task management endpoints.
type Handler struct {
	store Store
}

// NewHandler builds the handler. store may be nil; the routes then answer 503
// rather than panicking, keeping a deployment without a database serving the
// rest.
func NewHandler(store Store) *Handler {
	return &Handler{store: store}
}

// RegisterAuthed mounts every route behind the auth middleware, so each handler
// can rely on an authenticated user being on the request context.
func (h *Handler) RegisterAuthed(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	route(mux, auth, "GET /api/me/scheduled-tasks", h.list)
	route(mux, auth, "POST /api/me/scheduled-tasks", h.create)
	route(mux, auth, "GET /api/me/scheduled-tasks/{id}", h.get)
	route(mux, auth, "PUT /api/me/scheduled-tasks/{id}", h.update)
	route(mux, auth, "DELETE /api/me/scheduled-tasks/{id}", h.remove)
	route(mux, auth, "POST /api/me/scheduled-tasks/{id}/enable", h.enable)
	route(mux, auth, "POST /api/me/scheduled-tasks/{id}/disable", h.disable)
	route(mux, auth, "GET /api/me/scheduled-tasks/{id}/sessions", h.sessions)
	route(mux, auth, "POST /api/me/scheduled-tasks/{id}/sessions/clear", h.clearSessions)
}

// route mounts one pattern behind the auth middleware.
func route(mux *http.ServeMux, auth func(http.Handler) http.Handler, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, auth(h))
}

// caller returns the authenticated user; handlers behind RegisterAuthed rely on it.
func caller(r *http.Request) identity.User {
	u, _ := identity.UserFromContext(r.Context())
	return u
}
