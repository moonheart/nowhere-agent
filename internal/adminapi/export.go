package adminapi

import (
	"net/http"
	"time"

	"nowhere-agent/internal/audit"
)

// exportWriteTimeout is the rolling per-write deadline for the export stream,
// mirroring chatapi's SSE pattern: the server's WriteTimeout would otherwise
// abort a large export (a big account can take far longer than a normal
// response) and truncate the JSON download. A live write never hits it, while
// a half-open client's blocked write still ends the response. Best-effort — a
// server without deadline support is unaffected.
const exportWriteTimeout = 30 * time.Second

// rollingDeadlineWriter wraps w so every Write re-arms the write deadline
// before writing, exactly once per batch the export encoder emits. It is only
// an io.Writer (export.Service.Write takes io.Writer); the underlying
// ResponseWriter is what gets the new deadline.
type rollingDeadlineWriter struct {
	w http.ResponseWriter
}

func (d rollingDeadlineWriter) Write(p []byte) (int, error) {
	_ = http.NewResponseController(d.w).SetWriteDeadline(time.Now().Add(exportWriteTimeout))
	return d.w.Write(p)
}

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
	if err := h.exporter.Write(r.Context(), rollingDeadlineWriter{w: w}, u); err != nil {
		// The headers are already committed; the best a failed export can do is
		// leave a truncated download and a loud log. The audit record below
		// still lands (best-effort), so the failure is traceable.
		h.record(r, audit.Failure(audit.ActionMeExport).Target("user", u.ID).Detail(map[string]any{"err": err.Error()}))
		return
	}
	h.record(r, audit.Success(audit.ActionMeExport).Target("user", u.ID))
}
