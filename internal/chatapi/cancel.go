package chatapi

import (
	"encoding/json"
	"net/http"
)

// serveCancel handles POST /api/chat/cancel?threadId=<id>: it stops the
// session's active run, interrupting the agent loop and any in-flight tool
// calls, and marks the run cancelled. This is what the client's Stop button
// calls so a cancelled run terminates server-side rather than only closing the
// HTTP stream while the model keeps generating (and the sandbox keeps running).
func (h *Handler) serveCancel(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		http.Error(w, `{"error":"cancel unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	threadID := r.URL.Query().Get("threadId")
	if threadID == "" {
		http.Error(w, `{"error":"threadId required"}`, http.StatusBadRequest)
		return
	}
	if _, ok := h.authorizeSession(w, r, threadID); !ok {
		return
	}

	cancelled, err := h.runtime.CancelRun(r.Context(), threadID)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	if !cancelled {
		// No active run: nothing to stop. Report success-idempotent so the
		// client doesn't treat a late Stop (after the run finished) as an error.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": false})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
}
