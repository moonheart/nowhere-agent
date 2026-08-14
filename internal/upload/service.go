package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// Open returns a reader over one of the user's upload blobs (confined to
	// the user's own upload scope). It is the read half the data-export path
	// uses to embed image content into the document.
	Open(ctx context.Context, userID, id string) (io.ReadCloser, error)
	// Delete removes the user's upload. Returns ErrNotFound for a missing or
	// unowned upload, ErrReferenced when a message still uses the image, and
	// ErrBlobRemovalFailed when the record was deleted but its blob could not
	// be removed (a partial deletion — the record is gone either way).
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

// ErrBlobRemovalFailed reports a PARTIAL deletion: the upload record was
// deleted but its blob could not be removed. A retry of Delete would answer
// ErrNotFound (the record is gone), so this is terminal, not transient; it
// exists so a deletion is never reported as fully successful while an orphan
// blob remains. Quota counts records, so the orphan is never reclaimable by
// the user — the adminapi maps it to a 500 with an explicit message.
var ErrBlobRemovalFailed = errors.New("upload: record deleted but blob removal failed")

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

// Open returns a reader over the upload's blob bytes, resolved through the
// upload-scoped path ("uploads/<id>.webp") so a caller can never read outside
// their own upload scope. A missing or unowned blob surfaces as the blob
// store's not-found error.
func (s *Service) Open(ctx context.Context, userID, id string) (io.ReadCloser, error) {
	return s.blobs.OpenUserUpload(userID, "uploads/"+id+".webp")
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
	// nothing. When the blob removal then fails, the record is already gone:
	// surface it loudly instead of silently returning success — the caller
	// must not believe the data is fully gone while an orphan blob remains.
	if err := s.blobs.DeleteUserUpload(userID, id); err != nil {
		slog.Warn("upload: record deleted but blob removal failed", "user_id", userID, "id", id, "err", err)
		return fmt.Errorf("%w: %w", ErrBlobRemovalFailed, err)
	}
	return nil
}

// SweepOrphans is the hourly garbage collection for user-level upload blobs: a
// blob whose Delete failed after the record went (ErrBlobRemovalFailed is
// terminal — the record is gone, so a retry of Delete answers ErrNotFound) can
// never be reclaimed through the API. This pass scans the on-disk blob set and
// deletes blobs that have NO metadata row AND are not referenced by any
// message (both checks per blob, so a history image can never be swept out
// under a record). It is deliberately conservative: only unambiguous garbage
// is removed.
//
// Best-effort, matching the other hourly sweepers: a per-blob failure is
// logged and the pass continues; a store error (DB hiccup) aborts the pass so
// the next tick retries, and the caller's hourlySweep logs it. Returns the
// number of blobs removed.
func (s *Service) SweepOrphans(ctx context.Context, log *slog.Logger) (int, error) {
	byUser, err := s.blobs.ListUserUploads()
	if err != nil {
		return 0, err
	}
	var removed int
	for userID, ids := range byUser {
		for _, id := range ids {
			if _, err := s.store.Get(ctx, id); err == nil {
				continue // a metadata row owns this blob: keep
			} else if !errors.Is(err, ErrNotFound) {
				// DB hiccup: one user's pass must not silently sweep blind —
				// abort so the next tick retries against a healthy store.
				return removed, fmt.Errorf("look up upload %q: %w", id, err)
			}
			ref, err := s.store.ReferencedByMessage(ctx, userID, id)
			if err != nil {
				return removed, fmt.Errorf("check references for upload %q: %w", id, err)
			}
			if ref {
				continue // a message still references it: keep
			}
			if err := s.blobs.DeleteUserUpload(userID, id); err != nil {
				if log != nil {
					log.Warn("upload orphan sweep: blob removal failed", "user_id", userID, "id", id, "err", err)
				}
				continue
			}
			removed++
		}
	}
	return removed, nil
}
