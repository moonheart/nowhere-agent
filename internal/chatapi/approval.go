package chatapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"nowhere-agent/internal/session"
)

// serveApproval handles POST /api/chat/approval: the human verdict on a parked
// tool-approval (capability-gap O2). Body: {"approvalId": "...", "approved":
// bool}. The caller must own the session the approval belongs to (resolved
// server-side from the durable approval — the client never asserts ownership).
// On accept the run resumes on a fresh worker; the response is the verdict.
func (h *Handler) serveApproval(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		http.Error(w, `{"error":"approval unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ApprovalID string `json:"approvalId"`
		Approved   bool   `json:"approved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ApprovalID == "" {
		http.Error(w, `{"error":"approvalId required"}`, http.StatusBadRequest)
		return
	}

	// Resolve the approval to find its session, then enforce ownership before
	// acting on it (the decision endpoint must not let a caller reach another
	// user's parked run).
	ap, err := h.registry.ApprovalByID(r.Context(), body.ApprovalID)
	if err != nil {
		if errors.Is(err, session.ErrNoPendingApproval) {
			http.Error(w, `{"error":"approval not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	if _, ok := h.authorizeSession(w, r, ap.SessionID); !ok {
		return
	}

	run, err := h.registry.Resume(r.Context(), body.ApprovalID, body.Approved)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrNoPendingApproval):
			http.Error(w, `{"error":"approval already decided"}`, http.StatusConflict)
		case errors.Is(err, session.ErrRunNotWaiting):
			http.Error(w, `{"error":"run is not waiting for approval"}`, http.StatusConflict)
		case errors.Is(err, session.ErrRunActive):
			http.Error(w, `{"error":"a run is already active in this session"}`, http.StatusConflict)
		default:
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resumed": true, "runId": run.ID, "approved": body.Approved,
	})
}
