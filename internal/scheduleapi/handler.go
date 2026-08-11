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

	"nowhere-agent/internal/httpx"
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
	ListProducedSessions(ctx context.Context, taskID string) ([]schedule.ProducedSession, error)
	EndSessions(ctx context.Context, taskID string) (int, error)
}

// Runner fires one task immediately, out of band. *schedule.Trigger satisfies
// it via FireNow. A nil Runner means no provider is configured, so the run-now
// route answers 503 while the rest of task CRUD keeps working.
type Runner interface {
	FireNow(ctx context.Context, task schedule.Task) error
}

// Handler serves the scheduled-task management endpoints.
type Handler struct {
	store  Store
	runner Runner
	// targetValidator, when set, checks that a task's target_session_id (when
	// non-empty) belongs to the task owner BEFORE the task is stored. The
	// authoritative ownership gate runs again at fire time (schedule.Trigger.
	// resolveSession); this write-time check surfaces the error as a 400
	// instead of a confusing failed fire later.
	targetValidator func(ctx context.Context, userID, sessionID string) error
}

// NewHandler builds the handler. store may be nil; the routes then answer 503
// rather than panicking, keeping a deployment without a database serving the
// rest.
func NewHandler(store Store) *Handler {
	return &Handler{store: store}
}

// WithTargetValidator wires the write-time target-session ownership check
// (the server implements it over the session runtime). Nil skips the check.
func (h *Handler) WithTargetValidator(f func(ctx context.Context, userID, sessionID string) error) *Handler {
	h.targetValidator = f
	return h
}

// WithRunner wires the manual-run path (run-now). Nil leaves the route
// answering 503 (no LLM configured).
func (h *Handler) WithRunner(r Runner) *Handler {
	h.runner = r
	return h
}

// RegisterAuthed mounts every route onto the protected group. Auth is NOT
// wrapped per route: the group applies its middleware set once at Mount time,
// so this handler only declares which routes belong to the protected tier. Each
// handler relies on an authenticated user being on the request context.
func (h *Handler) RegisterAuthed(g *httpx.Router) {
	route(g, "GET /api/me/scheduled-tasks", h.list)
	route(g, "POST /api/me/scheduled-tasks", h.create)
	route(g, "GET /api/me/scheduled-tasks/{id}", h.get)
	route(g, "PUT /api/me/scheduled-tasks/{id}", h.update)
	route(g, "DELETE /api/me/scheduled-tasks/{id}", h.remove)
	route(g, "POST /api/me/scheduled-tasks/{id}/enable", h.enable)
	route(g, "POST /api/me/scheduled-tasks/{id}/disable", h.disable)
	route(g, "POST /api/me/scheduled-tasks/{id}/run", h.runNow)
	route(g, "GET /api/me/scheduled-tasks/{id}/sessions", h.sessions)
	route(g, "POST /api/me/scheduled-tasks/{id}/sessions/clear", h.clearSessions)
}

// route registers one pattern onto the protected group.
func route(g *httpx.Router, pattern string, h http.HandlerFunc) {
	g.HandleFunc(pattern, h)
}

// caller returns the authenticated user; handlers behind RegisterAuthed rely on it.
func caller(r *http.Request) identity.User {
	u, _ := identity.UserFromContext(r.Context())
	return u
}
