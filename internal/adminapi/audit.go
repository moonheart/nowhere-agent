package adminapi

import (
	"net/http"

	"nowhere-agent/internal/audit"
)

// listAudit is the platform-admin read side of the audit trail
// (enterprise-readiness P0): GET /api/admin/audit?action=&actor=&from=&to=&limit=&offset=.
// The trail is only queryable by a platform administrator — it names actors and
// targets, so it must not become a general-read surface.
func (h *Handler) listAudit(w http.ResponseWriter, r *http.Request) {
	if h.audit == nil {
		writeError(w, http.StatusServiceUnavailable, "audit trail unavailable")
		return
	}
	q := r.URL.Query()
	f := audit.Filter{
		Action: q.Get("action"),
		Actor:  q.Get("actor"),
		From:   timeParam(r, "from"),
		To:     timeParam(r, "to"),
		Limit:  intParam(r, "limit", audit.DefaultLimit, audit.MaxLimit),
		Offset: intParam(r, "offset", 0, 0),
	}
	entries, total, err := h.audit.List(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "audit query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
		"limit":   f.Limit,
		"offset":  f.Offset,
	})
}
