package adminapi

import (
	"encoding/json"
	"net/http"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/webhook"
)

// deliveryDTO is the wire shape of an outbox row (payload kept intact for
// inspection).
type deliveryDTO struct {
	ID            string          `json:"id"`
	RunID         string          `json:"run_id"`
	SessionID     string          `json:"session_id"`
	TargetURL     string          `json:"target_url"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	DeliveredAt   *time.Time      `json:"delivered_at,omitempty"`
}

func deliveryDTOOf(d webhook.Delivery) deliveryDTO {
	return deliveryDTO{
		ID: d.ID, RunID: d.RunID, SessionID: d.SessionID, TargetURL: d.TargetURL,
		Payload: d.Payload, Status: d.Status, Attempts: d.Attempts,
		NextAttemptAt: d.NextAttemptAt, LastError: d.LastError,
		CreatedAt: d.CreatedAt, DeliveredAt: d.DeliveredAt,
	}
}

// listDeliveries: GET /api/admin/webhook-deliveries?status=&limit=&offset=.
func (h *Handler) listDeliveries(w http.ResponseWriter, r *http.Request) {
	if h.deliveries == nil {
		writeError(w, http.StatusServiceUnavailable, "webhook outbox not enabled")
		return
	}
	status := r.URL.Query().Get("status")
	limit := intParam(r, "limit", 50, 200)
	offset := intParam(r, "offset", 0, 0)
	rows, total, err := h.deliveries.List(r.Context(), status, limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]deliveryDTO, 0, len(rows))
	for _, d := range rows {
		out = append(out, deliveryDTOOf(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out, "total": total})
}

// requeueDelivery: POST /api/admin/webhook-deliveries/{id}/retry — resets a
// dead-lettered delivery to pending so the sweeper tries it again.
func (h *Handler) requeueDelivery(w http.ResponseWriter, r *http.Request) {
	if h.deliveries == nil {
		writeError(w, http.StatusServiceUnavailable, "webhook outbox not enabled")
		return
	}
	id := r.PathValue("id")
	if err := h.deliveries.Requeue(r.Context(), id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionWebhookDeliveryRequeue).Target("webhook_delivery", id))
	w.WriteHeader(http.StatusNoContent)
}
