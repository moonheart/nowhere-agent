// Package workspace implements workspace-persistence (design D4): the
// per-session workspace lives on durable external storage and is materialized
// into a sandbox at start and solidified back at end. Containers are
// disposable; the workspace is durable. Movement follows a sync (push/pull)
// model, and solidify is atomic via two-stage commit.
package workspace

import "time"

// Ref identifies a persisted workspace version.
type Ref struct {
	// SessionID scopes the workspace to a session.
	SessionID string
	// Version is a monotonically increasing identifier for a solidified snapshot.
	Version int64
}

// Meta describes a stored workspace version.
type Meta struct {
	Ref       Ref
	CreatedAt time.Time
	// Files is the number of files in this version.
	Files int
	// Bytes is the total size in bytes.
	Bytes int64
}

// Store is the pluggable durable-backend abstraction. The built-in backend is
// a local directory; an S3-compatible backend implements the same contract.
//
// Materialize/solidify are expressed against a local filesystem path because
// the sandbox mount point is a local dir; remote backends translate to/from
// object storage inside these methods.
type Store interface {
	// Solidify atomically persists the contents of dir as a new version and
	// returns its Ref. If the write is interrupted, the previously committed
	// version remains current (no partial state).
	Solidify(sessionID, dir string) (Ref, error)

	// Materialize extracts the current version of the session's workspace into
	// dir, creating it if needed. If no version exists, dir is left empty.
	Materialize(sessionID, dir string) (Ref, error)

	// Current returns the latest committed version for a session, or false.
	Current(sessionID string) (Ref, bool)

	// Delete removes all versions for a session.
	Delete(sessionID string) error
}
