package upload

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"nowhere-agent/internal/workspace"
)

// Uploader is the orchestration boundary the HTTP layers consume: upload a new
// user-level image, list the caller's uploads, delete one (with reference
// protection). *Service implements it.
type Uploader interface {
	// Upload stores raw image bytes as a user-level upload and returns its
	// record. The image is validated + WebP-normalized by the blob store.
	Upload(ctx context.Context, userID, name string, raw []byte) (Upload, error)
	// List returns the user's uploads, newest first.
	List(ctx context.Context, userID string) ([]Upload, error)
	// Delete removes the user's upload. Returns ErrNotFound for a missing or
	// unowned upload and ErrReferenced when a message still uses the image.
	Delete(ctx context.Context, userID, id string) error
}

// Service wires the metadata store and the blob store into one orchestration.
type Service struct {
	store Store
	blobs *workspace.ImageStore
}

// NewService builds the upload service over a record store and the workspace
// blob store.
func NewService(store Store, blobs *workspace.ImageStore) *Service {
	return &Service{store: store, blobs: blobs}
}

var _ Uploader = (*Service)(nil)

// Upload validates the image bytes via the blob store (which WebP-normalizes),
// then records the metadata row under the same id the blob path carries.
func (s *Service) Upload(ctx context.Context, userID, name string, raw []byte) (Upload, error) {
	path, size, err := s.blobs.SaveUserUpload(userID, name, raw)
	if err != nil {
		return Upload{}, err
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, "uploads/"), ".webp")
	u := Upload{
		ID:        id,
		UserID:    userID,
		Filename:  filepath.Base(filepath.Clean(name)),
		Size:      size,
		MediaType: "image/webp",
		CreatedAt: time.Now().UTC(),
	}
	return s.store.Create(ctx, u)
}

// List returns the caller's uploads, newest first.
func (s *Service) List(ctx context.Context, userID string) ([]Upload, error) {
	return s.store.ListByUser(ctx, userID)
}

// Delete removes the caller's upload. Ownership is enforced (a foreign id is a
// 404, not a 403, so upload ids cannot be enumerated), and a referenced upload
// is rejected rather than allowed to break history.
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	u, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.UserID != userID {
		return ErrNotFound
	}
	ref, err := s.store.ReferencedByMessage(ctx, id)
	if err != nil {
		return err
	}
	if ref {
		return ErrReferenced
	}
	if err := s.store.Delete(ctx, id); err != nil {
		return err
	}
	// Remove the blob after the record, so a blob-store hiccup leaves a record
	// that a retry can still resolve rather than an orphaned row pointing at
	// nothing. The record is gone either way once this returns.
	return s.blobs.DeleteUserUpload(userID, id)
}
