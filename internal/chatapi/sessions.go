package chatapi

import (
	"encoding/json"
	"net/http"
	"time"

	"nowhere-agent/internal/identity"
)

// sessionDTO is one conversation in the sidebar list.
type sessionDTO struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// serveSessions handles GET /api/chat/sessions: it lists the caller's sessions
// (most-recently-active first) for the sidebar conversation list. Unlike
// history/resume (which authorize a specific session), this scopes the query to
// the authenticated user, so it only ever returns their own conversations.
func (h *Handler) serveSessions(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		http.Error(w, `{"error":"sessions unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	sessions, err := h.runtime.ListSessionsByUser(r.Context(), user.ID)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	out := make([]sessionDTO, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, sessionDTO{ID: s.ID, Title: s.Title, UpdatedAt: s.UpdatedAt})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sessions": out})
}

// serveDeleteSession handles DELETE /api/chat/sessions/{id}: it soft-deletes
// (ends) a session the caller owns, removing it from their sidebar. It returns
// 404 when the session doesn't exist or belongs to someone else (indistinguishable
// to avoid leaking existence), 204 on success.
func (h *Handler) serveDeleteSession(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		http.Error(w, `{"error":"sessions unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
		return
	}

	deleted, err := h.runtime.DeleteSessionForUser(r.Context(), id, user.ID)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
