package adminapi

import (
	"context"
	"log/slog"
	"net/http"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/workspace"
)

// This file holds the platform purge routes (P2-8 no-data-hard-delete): the
// destructive half of data governance. Soft deletes keep rows around for
// recovery; these routes remove them for real, so every one is admin-gated,
// audited, and scoped to a single id — there is deliberately no bulk delete.

// SessionPurgeStore is the session-destruction surface the admin routes need.
// *session.PGStore satisfies it; the interface keeps the console free of the
// session store's full contract.
type SessionPurgeStore interface {
	DeleteSession(ctx context.Context, id string) error
	SessionIDsForUser(ctx context.Context, userID string) ([]string, error)
}

// ImagePurger removes workspace images for hard-deleted data. *workspace.
// ImageStore satisfies it; nil-safe (a deployment without a workspace dir has
// no images to remove).
type ImagePurger interface {
	DeleteSessionImages(sessionID string) error
	DeleteUserUploadScope(userID string) error
}

// RunCancellor stops a session's in-flight run worker. *session.RunRegistry
// satisfies it (Cancel is transport-independent and interrupts the loop, so
// token burn stops promptly). Nil-safe: a purge without one skips the cancel
// and proceeds with the delete.
type RunCancellor interface {
	Cancel(sessionID string) bool
}

// WithPurge wires the hard-delete routes (platform purge): sessions
// (DELETE /api/admin/sessions/{id}) and the image cleanup that rides on user
// deletion. runs stops an active run before the session row goes (nil skips
// the cancel). Left sessions nil, the session purge answers 503; image cleanup
// is skipped.
func (h *Handler) WithPurge(s SessionPurgeStore, images ImagePurger, runs RunCancellor) *Handler {
	h.sessions = s
	h.images = images
	h.runs = runs
	return h
}

// deleteSession hard-deletes one session and, when an image store is wired,
// its workspace image dir. Cascades remove the runs/messages/events/approvals
// rows; the image dir is keyed by session id and would otherwise orphan (the
// retention sweep lists sessions from the DB, which no longer has this row).
func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session purge unavailable")
		return
	}
	id := r.PathValue("id")
	// Stop an in-flight run BEFORE the hard delete. The cascade would otherwise
	// rip the run's rows out from under the worker, which fails its next write
	// with a bogus FK error while the LLM stream keeps spending. Cancelling
	// first interrupts the loop and the worker settles cancelled; its leftover
	// writes then fail harmlessly — those rows are going anyway.
	if h.runs != nil {
		h.runs.Cancel(id)
	}
	if err := h.sessions.DeleteSession(r.Context(), id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.purgeSessionImages(r, id)
	h.record(r, audit.Success(audit.ActionAdminSessionDelete).Target("session", id))
	w.WriteHeader(http.StatusNoContent)
}

// purgeSessionImages removes one session's workspace image dir. Best-effort: a
// failure is logged, never allowed to fail the request — the DB row is already
// gone and the leak is a disk concern, not a data-integrity one.
func (h *Handler) purgeSessionImages(r *http.Request, sessionID string) {
	if h.images == nil {
		return
	}
	if err := h.images.DeleteSessionImages(sessionID); err != nil {
		slog.Warn("purge: session image cleanup failed", "session", sessionID, "err", err)
	}
}

// purgeUserImages removes a deleted user's upload scope and every session
// image dir. Called AFTER the user row is gone (its session ids were captured
// before deletion). Best-effort like purgeSessionImages.
func (h *Handler) purgeUserImages(r *http.Request, userID string, sessionIDs []string) {
	if h.images == nil {
		return
	}
	for _, id := range sessionIDs {
		if err := h.images.DeleteSessionImages(id); err != nil {
			slog.Warn("purge: session image cleanup failed", "session", id, "err", err)
		}
	}
	if err := h.images.DeleteUserUploadScope(userID); err != nil {
		slog.Warn("purge: user upload scope cleanup failed", "user", userID, "err", err)
	}
}

var (
	_ SessionPurgeStore = (*session.PGStore)(nil)
	_ ImagePurger       = (*workspace.ImageStore)(nil)
)
