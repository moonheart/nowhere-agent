package chatapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/upload"
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

	body, err := httpx.ReadBodyMax(r, maxImageUploadBytes)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "image too large")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "read body failed")
		return
	}

	// Enforce the per-session image quota BEFORE any blob is written (the same
	// check-then-write shape as the user-level upload quota): the session's
	// stored images are counted from disk, so an over-quota attempt costs
	// nothing. Session images and the session's sandbox workspace share the
	// session dir; SessionUsage counts only WebP files, the format Save
	// normalizes to.
	if h.imageQuota != nil {
		q := h.imageQuota()
		if q.MaxFiles > 0 || q.MaxBytes > 0 {
			files, used, err := h.images.SessionUsage(sessID)
			if err != nil {
				http.Error(w, `{"error":"image save failed"}`, http.StatusInternalServerError)
				return
			}
			if q.MaxFiles > 0 && files >= q.MaxFiles {
				http.Error(w, `{"error":"upload quota exceeded"}`, http.StatusRequestEntityTooLarge)
				return
			}
			if q.MaxBytes > 0 && used+int64(len(body)) > q.MaxBytes {
				http.Error(w, `{"error":"upload quota exceeded"}`, http.StatusRequestEntityTooLarge)
				return
			}
		}
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

// serveUserImageUpload handles POST /api/chat/uploads (change
// user-image-uploads): a session-independent, user-scoped image upload. It
// stores the image via the upload service (WebP-normalized blob + metadata
// record) and returns the "uploads/<id>.webp" reference the frontend then
// includes as an image part in the next chat message — including a brand-new
// conversation's first message, which has no session yet.
func (h *Handler) serveUserImageUpload(w http.ResponseWriter, r *http.Request) {
	if h.uploads == nil {
		http.Error(w, `{"error":"image upload unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	u, ok := identity.UserFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	body, err := httpx.ReadBodyMax(r, maxImageUploadBytes)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "image too large")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "read body failed")
		return
	}

	name := "upload.png"
	if fn := r.URL.Query().Get("name"); fn != "" {
		name = fn
	}
	up, err := h.uploads.Upload(r.Context(), u.ID, name, body)
	if err != nil {
		if errors.Is(err, workspace.ErrUnsupportedImage) {
			http.Error(w, `{"error":"unsupported or malformed image"}`, http.StatusUnsupportedMediaType)
			return
		}
		if errors.Is(err, upload.ErrQuotaExceeded) {
			http.Error(w, `{"error":"upload quota exceeded"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"image save failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"path": "uploads/" + up.ID + ".webp"})
}

// serveUserFile handles GET /api/chat/uploads/{id}: it streams a user-level
// image blob to its owner. History rendering resolves "uploads/…" message paths
// here. Ownership is enforced by the blob layout (the id resolves under the
// caller's own upload scope), so a guessed id from another user 404s.
func (h *Handler) serveUserFile(w http.ResponseWriter, r *http.Request) {
	if h.images == nil {
		http.Error(w, `{"error":"files unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	u, ok := identity.UserFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	rc, err := h.images.OpenUserUpload(u.ID, "uploads/"+r.PathValue("id"))
	if err != nil {
		// Missing file or rejected (malformed/escape) — indistinguishable to the
		// caller, so we don't leak upload layout.
		http.Error(w, `{"error":"file not found"}`, http.StatusNotFound)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, rc)
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
