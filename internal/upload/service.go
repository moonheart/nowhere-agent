package upload

import (
	"context"
	"errors"
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

// Quota is one user's upload budget, read live at each upload (nil reader or
// zero caps mean unlimited). The count cap bounds the metadata rows, the byte
// cap bounds the blob store; either being hit rejects the upload with
// ErrQuotaExceeded.
type Quota struct {
	MaxFiles int   // per-user upload records; <= 0 = unlimited
	MaxBytes int64 // per-user total stored bytes; <= 0 = unlimited
}

// Service wires the metadata store and the blob store into one orchestration.
type Service struct {
	store Store
	blobs *workspace.ImageStore
	quota func() Quota
}

// NewService builds the upload service over a record store and the workspace
// blob store. quota, when non-nil, is read on every Upload to enforce the
// per-user cap (so an admin-console retune applies without a restart).
func NewService(store Store, blobs *workspace.ImageStore, quota func() Quota) *Service {
	return &Service{store: store, blobs: blobs, quota: quota}
}

var _ Uploader = (*Service)(nil)

// ErrQuotaExceeded reports that the caller's upload quota is exhausted. The
// upload endpoint maps it to 413.
var ErrQuotaExceeded = errors.New("upload: per-user quota exceeded")

// Upload validates the image bytes via the blob store (which WebP-normalizes),
// then records the metadata row under the same id the blob path carries. The
// per-user quota is enforced BEFORE any blob is written, so an over-quota
// attempt costs nothing. If the record insert fails after the blob landed, the
// blob is removed again — a failed upload must not leak an orphan blob (quota
// counts only records, so an orphan would also be unreclaimable by the user).
func (s *Service) Upload(ctx context.Context, userID, name string, raw []byte) (Upload, error) {
	if s.quota != nil {
		q := s.quota()
		if q.MaxFiles > 0 || q.MaxBytes > 0 {
			if err := s.checkQuota(ctx, userID, q, int64(len(raw))); err != nil {
				return Upload{}, err
			}
		}
	}
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
	created, err := s.store.Create(ctx, u)
	if err != nil {
		// Compensate: the blob is written but the record never existed, so no
		// retry of Create can ever reference it. Best-effort — a cleanup
		// failure is logged by the blob store, and the orphan is bounded (one
		// file per failed insert).
		_ = s.blobs.DeleteUserUpload(userID, id)
		return Upload{}, err
	}
	return created, nil
}

// checkQuota rejects the upload when the user already holds the max file count
// or when the current total plus the incoming raw bytes would exceed the byte
// cap. The totals come from the metadata store — the authoritative record of
// what the user owns (orphaned blobs count nothing, which is the correct
// incentive: quota is about owned records).
//
// The check is snapshot-based: it reads a ListByUser snapshot and the blob is
// written and recorded AFTERWARDS, so concurrent uploads can each pass the
// check and overshoot the cap slightly. That is an accepted cost ceiling, not
// an exact limit — strict atomicity would serialize uploads for no real gain.
func (s *Service) checkQuota(ctx context.Context, userID string, q Quota, incoming int64) error {
	uploads, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	if q.MaxFiles > 0 && len(uploads) >= q.MaxFiles {
		return ErrQuotaExceeded
	}
	if q.MaxBytes > 0 {
		var total int64
		for _, u := range uploads {
			total += u.Size
		}
		if total+incoming > q.MaxBytes {
			return ErrQuotaExceeded
		}
	}
	return nil
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
	ref, err := s.store.ReferencedByMessage(ctx, userID, id)
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
