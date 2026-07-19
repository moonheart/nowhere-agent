package chatapi

import (
	"io"
	"net/http"
)

// serveFile handles GET /api/chat/sessions/{id}/files/{path...}: it streams a
// workspace image to the session owner. The message stream references images by
// path rather than inlining base64 (persist-raw-messages D6), so the frontend
// resolves them here. Authorization is the same session-ownership check used by
// history/resume; the read itself is confined to the session's workspace dir by
// the ImageStore (path traversal is rejected there too — defense in depth).
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil || h.images == nil {
		http.Error(w, `{"error":"files unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	sessID := r.PathValue("id")
	if sessID == "" {
		http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
		return
	}
	if _, ok := h.authorizeSession(w, r, sessID); !ok {
		return // 404/403 already written
	}

	rel := r.PathValue("path")
	rc, err := h.images.Open(sessID, rel)
	if err != nil {
		// Missing file or rejected (escape/absolute) — indistinguishable to the
		// caller, so we don't leak workspace layout.
		http.Error(w, `{"error":"file not found"}`, http.StatusNotFound)
		return
	}
	defer rc.Close()

	// Images are stored normalized to WebP (see workspace.ImageStore.Save).
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, rc)
}
