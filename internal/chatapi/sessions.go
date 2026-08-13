package chatapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
)

// sessionDTO is one conversation in the sidebar list.
type sessionDTO struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Default and cap for the sidebar conversation list page size.
const (
	defaultSessionPageSize = 25
	maxSessionPageSize     = 100
)

// sessionCursorCodec is the wire format of the opaque pagination cursor: the
// (updated_at, id) keyset position of the page's last session, base64url JSON.
type sessionCursorCodec struct {
	UpdatedAt string `json:"u"`
	ID        string `json:"id"`
}

func encodeSessionCursor(c session.SessionCursor) string {
	raw, _ := json.Marshal(sessionCursorCodec{UpdatedAt: c.UpdatedAt.Format(time.RFC3339Nano), ID: c.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeSessionCursor(raw string) (*session.SessionCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("cursor is not base64url")
	}
	var c sessionCursorCodec
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, errors.New("cursor is malformed")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, c.UpdatedAt)
	if err != nil || c.ID == "" {
		return nil, errors.New("cursor is malformed")
	}
	return &session.SessionCursor{UpdatedAt: updatedAt, ID: c.ID}, nil
}

// serveSessions handles GET /api/chat/sessions: it lists the caller's sessions
// (most-recently-active first) for the sidebar conversation list, one page at a
// time. limit caps the page size (default 25); cursor is the previous page's
// nextCursor (omitted for the first page). q, when non-empty, narrows the list
// to sessions whose title contains it (case-insensitive) — the sidebar search,
// served from the backend so old pages are searchable, not just the ones the
// client has loaded. The response's nextCursor is empty when the list is
// exhausted. Unlike history/resume (which authorize a specific session), this
// scopes the query to the authenticated user, so it only ever returns their
// own conversations.
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

	limit := defaultSessionPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, maxSessionPageSize)
		}
	}
	var cursor *session.SessionCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeSessionCursor(raw)
		if err != nil {
			http.Error(w, `{"error":"invalid cursor"}`, http.StatusBadRequest)
			return
		}
		cursor = c
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	page, err := h.runtime.ListSessionsByUser(r.Context(), user.ID, q, limit, cursor)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]sessionDTO, 0, len(page.Sessions))
	for _, s := range page.Sessions {
		out = append(out, sessionDTO{ID: s.ID, Title: s.Title, UpdatedAt: s.UpdatedAt})
	}
	resp := map[string]any{"sessions": out, "nextCursor": ""}
	if page.NextCursor != nil {
		resp["nextCursor"] = encodeSessionCursor(*page.NextCursor)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// serveDeleteSession handles DELETE /api/chat/sessions/{id}: it soft-deletes
// (ends) a session the caller owns, removing it from their sidebar. An active
// run is cancelled first, so a deleted conversation does not keep generating
// headless in the background. It returns 404 when the session doesn't exist or
// belongs to someone else (indistinguishable to avoid leaking existence), 204
// on success.
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

	// A session with an in-flight run must be cancelled before the soft delete:
	// ending the session only hides it from the sidebar, while the run would
	// otherwise keep executing headless — model generating, tools running —
	// until it settles on its own. Cancel is transport-independent (the same
	// path the Stop button uses). It runs only after the ownership check, so a
	// DELETE on a foreign session cannot cancel someone else's run; existence
	// and ownership stay indistinguishable (both 404), matching the
	// DeleteSessionForUser contract below.
	if s, err := h.runtime.GetSession(r.Context(), id); err == nil && sessionVisibleTo(s, user.ID) && h.registry != nil {
		h.registry.Cancel(id)
	}

	deleted, err := h.runtime.DeleteSessionForUser(r.Context(), id, user.ID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveSessionActive handles GET /api/chat/sessions/{id}/active: it reports
// whether a run is currently in flight for the session as {active: bool}. It is
// the lightweight replacement for the idle poll's /history call — the poll runs
// every few seconds per idle tab, and /history rebuilds the whole conversation
// (messages + pending approvals + session state) each time, which is wasted DB
// work for a yes/no flag. The check is memory-first: the run registry's worker
// map is the authoritative in-process view (the poll's purpose is detecting a
// run started by another tab of the same gateway), so the common case never
// touches Postgres; the durable ActiveRun query is the fallback for a registry
// without the worker (tests/dev) and agrees with the /history active flag.
func (h *Handler) serveSessionActive(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		http.Error(w, `{"error":"sessions unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, `{"error":"session id required"}`, http.StatusBadRequest)
		return
	}
	if _, ok := h.authorizeSession(w, r, sessionID); !ok {
		return
	}

	active := false
	if h.registry != nil {
		active = h.registry.ActiveWorker(sessionID)
	}
	if !active {
		if _, a, err := h.runtime.ActiveRun(r.Context(), sessionID); err == nil {
			active = a
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"active": active})
}
