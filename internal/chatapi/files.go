package chatapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"nowhere-agent/internal/workspace"
)

// maxImageUploadBytes caps the raw upload payload size (image-input capability):
// screenshots/diagrams fit comfortably; the store re-encodes to WebP to bound
// the durable size further.
const maxImageUploadBytes = 10 << 20 // 10 MiB

// serveImageUpload handles POST /api/chat/sessions/{id}/images: it reads the
// raw image payload from the request body, stores it via the session's
// ImageStore (WebP-normalized), and returns the session-relative path the
// frontend then includes as an image part in the next chat message.
func (h *Handler) serveImageUpload(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil || h.images == nil {
		http.Error(w, `{"error":"image upload unavailable"}`, http.StatusServiceUnavailable)
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxImageUploadBytes+1))
	if err != nil {
		http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
		return
	}
	if len(body) > maxImageUploadBytes {
		http.Error(w, `{"error":"image too large"}`, http.StatusRequestEntityTooLarge)
		return
	}

	// The store decodes the payload (PNG/JPEG/GIF/WebP), rejects unsupported or
	// malformed bytes, and re-encodes to WebP under the session dir.
	name := "upload.webp"
	if fn := r.URL.Query().Get("name"); fn != "" {
		name = fn
	}
	rel, err := h.images.Save(sessID, name, body)
	if err != nil {
		if errors.Is(err, workspace.ErrUnsupportedImage) {
			http.Error(w, `{"error":"unsupported or malformed image"}`, http.StatusUnsupportedMediaType)
			return
		}
		http.Error(w, `{"error":"image save failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"path": rel})
}

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
