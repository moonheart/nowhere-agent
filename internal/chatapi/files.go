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

	"github.com/google/uuid"
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
		httpx.Error(w, http.StatusServiceUnavailable, "image upload unavailable")
		return
	}
	sessID := r.PathValue("id")
	if sessID == "" {
		httpx.Error(w, http.StatusBadRequest, "id required")
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
				httpx.Error(w, http.StatusInternalServerError, "image save failed")
				return
			}
			if q.MaxFiles > 0 && files >= q.MaxFiles {
				httpx.Error(w, http.StatusRequestEntityTooLarge, "upload quota exceeded")
				return
			}
			if q.MaxBytes > 0 && used+int64(len(body)) > q.MaxBytes {
				httpx.Error(w, http.StatusRequestEntityTooLarge, "upload quota exceeded")
				return
			}
		}
	}

	// The store decodes the payload (PNG/JPEG/GIF/WebP), rejects unsupported or
	// malformed bytes, and re-encodes to WebP under the session dir. The file
	// name is ALWAYS a fresh uuid: the frontend sends no name, and a client-
	// supplied fixed name would let consecutive uploads overwrite each other,
	// silently re-pointing old message references at the newest image (the
	// same convention as user-level uploads).
	name := uuid.NewString()
	rel, err := h.images.Save(sessID, name, body)
	if err != nil {
		if errors.Is(err, workspace.ErrUnsupportedImage) {
			httpx.Error(w, http.StatusUnsupportedMediaType, "unsupported or malformed image")
			return
		}
		if errors.Is(err, workspace.ErrImagePixelLimit) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "image too large")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "image save failed")
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
		httpx.Error(w, http.StatusServiceUnavailable, "image upload unavailable")
		return
	}
	u, ok := identity.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
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
			httpx.Error(w, http.StatusUnsupportedMediaType, "unsupported or malformed image")
			return
		}
		if errors.Is(err, workspace.ErrImagePixelLimit) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "image too large")
			return
		}
		if errors.Is(err, upload.ErrQuotaExceeded) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "upload quota exceeded")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "image save failed")
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
		httpx.Error(w, http.StatusServiceUnavailable, "files unavailable")
		return
	}
	u, ok := identity.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	rc, err := h.images.OpenUserUpload(u.ID, "uploads/"+r.PathValue("id"))
	if err != nil {
		// Missing file or rejected (malformed/escape) — indistinguishable to the
		// caller, so we don't leak upload layout.
		httpx.Error(w, http.StatusNotFound, "file not found")
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
		httpx.Error(w, http.StatusServiceUnavailable, "files unavailable")
		return
	}
	sessID := r.PathValue("id")
	if sessID == "" {
		httpx.Error(w, http.StatusBadRequest, "id required")
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
		httpx.Error(w, http.StatusNotFound, "file not found")
		return
	}
	defer rc.Close()

	// Images are stored normalized to WebP (see workspace.ImageStore.Save).
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, rc)
}
