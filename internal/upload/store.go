// Package upload manages user-level image uploads (change user-image-uploads):
// images uploaded independently of any session, so the first message of a chat
// can carry one. The WebP blob lives in the workspace store
// (<root>/__uploads__/<user_id>/<id>.webp); this package owns the metadata
// record (the uploads table), the upload/list/delete orchestration, and the
// reference protection that keeps history images intact.
package upload

import (
	"context"
	"errors"
	"time"
)

// Upload is the metadata record for one user-level image.
type Upload struct {
	ID        string
	UserID    string
	Filename  string
	Size      int64
	MediaType string
	CreatedAt time.Time
}

// Errors surfaced across the boundary. Callers map ErrNotFound to 404 and
// ErrReferenced to 409.
var (
	// ErrNotFound reports a missing upload (or one the caller does not own).
	ErrNotFound = errors.New("upload: not found")
	// ErrReferenced reports a delete attempt on an upload a message still uses.
	ErrReferenced = errors.New("upload: image is referenced by a message")
)

// Store is the metadata-record boundary. Blob I/O is the workspace
// ImageStore's job; this store only tracks rows.
type Store interface {
	// Create inserts an upload record and returns it.
	Create(ctx context.Context, u Upload) (Upload, error)
	// ListByUser returns one user's uploads, newest first.
	ListByUser(ctx context.Context, userID string) ([]Upload, error)
	// Get returns one upload by id.
	Get(ctx context.Context, id string) (Upload, error)
	// Delete removes the record by id.
	Delete(ctx context.Context, id string) error
	// ReferencedByMessage reports whether any stored message references the
	// upload id (its content JSON embeds "uploads/<id>.webp").
	ReferencedByMessage(ctx context.Context, id string) (bool, error)
}
