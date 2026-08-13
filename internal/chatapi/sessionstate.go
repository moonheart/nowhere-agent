package chatapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"nowhere-agent/internal/httpx"
)

// serveSetSessionState handles POST /api/chat/sessions/{id}/state: it writes ONE
// key of the session's generic state store on the owner's behalf. This is the
// out-of-run counterpart to the plan_write tool's in-run write — used for
// client-driven session settings (e.g. permission_mode) that are not produced by
// a tool call. The write goes through Runtime.SetSessionStateKV so it persists
// AND fans out a live session_state frame, keeping every attached client in sync.
//
// Keys are allow-listed: the generic store is also the model's plan scratchpad,
// so an arbitrary client write could clobber it. Only client-owned settings keys
// are accepted here.
func (h *Handler) serveSetSessionState(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		http.Error(w, `{"error":"state unavailable"}`, http.StatusServiceUnavailable)
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

	var body struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	// Bound the state write at 4 MiB, the same as the chat request body —
	// the value is a settings blob, never a file payload.
	raw, err := httpx.ReadBodyMax(r, 4<<20)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			http.Error(w, `{"error":"payload too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if !clientSettableStateKey(body.Key) {
		http.Error(w, `{"error":"key not client-settable"}`, http.StatusForbidden)
		return
	}
	if len(body.Value) == 0 {
		body.Value = json.RawMessage(`null`)
	}

	if err := h.runtime.SetSessionStateKV(r.Context(), sessionID, body.Key, body.Value); err != nil {
		slog.Warn("set session state", "session", sessionID, "key", body.Key, "err", err)
		httpx.ErrorFrom(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "key": body.Key})
}

// clientSettableStateKey reports whether a session-state key may be written via
// the client state endpoint. The store is shared with the model's plan/tool
// scratchpad, so only keys the client owns are open to direct writes.
func clientSettableStateKey(key string) bool {
	switch key {
	case PermissionModeStateKey:
		return true
	default:
		return false
	}
}
