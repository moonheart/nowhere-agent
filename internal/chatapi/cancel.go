package chatapi

import (
	"encoding/json"
	"net/http"

	"nowhere-agent/internal/httpx"
)

// serveCancel handles POST /api/chat/cancel?threadId=<id>: it stops the
// session's active run, interrupting the agent loop and any in-flight tool
// calls, and marks the run cancelled. This is what the client's Stop button
// calls so a cancelled run terminates server-side rather than only closing the
// HTTP stream while the model keeps generating (and the sandbox keeps running).
func (h *Handler) serveCancel(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "cancel unavailable")
		return
	}
	threadID := r.URL.Query().Get("threadId")
	if threadID == "" {
		httpx.Error(w, http.StatusBadRequest, "threadId required")
		return
	}
	if _, ok := h.authorizeSession(w, r, threadID); !ok {
		return
	}

	// Cancel is transport-independent (D4): the registry interrupts the run's
	// worker goroutine regardless of which client submitted or is attached.
	cancelled := h.registry.Cancel(threadID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": cancelled})
}
