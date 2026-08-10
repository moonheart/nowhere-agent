package adminapi

import (
	"net/http"
	"time"

	"nowhere-agent/internal/audit"
)

// exportData streams the caller's full data footprint as a JSON download
// (data governance / GDPR-style export): GET /api/me/export. Self-service and
// confined to the requesting user — there is no "export someone else" surface.
func (h *Handler) exportData(w http.ResponseWriter, r *http.Request) {
	if h.exporter == nil {
		writeError(w, http.StatusServiceUnavailable, "data export unavailable")
		return
	}
	u := caller(r)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		`attachment; filename="nowhere-export-`+time.Now().UTC().Format("20060102T150405Z")+`.json"`)
	if err := h.exporter.Write(r.Context(), w, u); err != nil {
		// The headers are already committed; the best a failed export can do is
		// leave a truncated download and a loud log. The audit record below
		// still lands (best-effort), so the failure is traceable.
		h.record(r, audit.Failure(audit.ActionMeExport).Target("user", u.ID).Detail(map[string]any{"err": err.Error()}))
		return
	}
	h.record(r, audit.Success(audit.ActionMeExport).Target("user", u.ID))
}
