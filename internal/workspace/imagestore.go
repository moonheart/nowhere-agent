package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoding
	_ "image/jpeg" // register JPEG decoding
	_ "image/png"  // register PNG decoding
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gen2brain/webp"
	"github.com/google/uuid"

	"nowhere-agent/internal/provider"
)

// Reserved root-relative directory for user-level upload blobs and the path
// prefix messages reference them by. The dir name cannot collide with a session
// id (UUIDs), and the prefix is unambiguous because every user-level upload
// returns "uploads/<uuid>.webp".
const (
	uploadsDir        = "__uploads__"
	userUploadPrefix  = "uploads/"
	userUploadSuffix  = ".webp"
	maxUploadFilename = 200
)

// ImageStore persists image payloads as WebP files under a per-session
// directory, and reads them back by workspace-relative path with confinement.
// It is the durable home for the full image bytes that conversation messages
// reference by path (persist-raw-messages D6): the messages table holds only
// the pointer, this store holds the payload.
//
// Layout: <root>/<sessionID>/<name>.webp. Paths handed back and accepted for
// reads are session-relative (no session prefix), and every read/write is
// confined to the session's directory — absolute paths, ".." escapes, and
// symlink escapes are rejected.
//
// User-level uploads (change user-image-uploads) live separately under
// <root>/__uploads__/<userID>/<id>.webp and are referenced as
// "uploads/<id>.webp". Their blob I/O goes through the internal blobStore
// seam so the backend can be swapped (local now, S3-compatible later — the
// same convention workspace.Store documents for sandbox workspaces).
type ImageStore struct {
	root  string
	blobs blobStore
}

// NewImageStore creates an ImageStore rooted at dir (created on demand).
func NewImageStore(root string) *ImageStore {
	return &ImageStore{root: root, blobs: &localBlobStore{root: root}}
}

// Root returns the store's configured root directory (tests and operators
// inspecting disk layout).
func (s *ImageStore) Root() string { return s.root }

// blobStore abstracts the durable blob read/write for user-level uploads. The
// shipped implementation is a local directory; an S3-compatible backend
// implements the same contract (mirrors the workspace.Store convention).
type blobStore interface {
	// Put writes a user's blob under its id, replacing any existing bytes.
	Put(userID, id string, data []byte) error
	// Open reads a user's blob by id.
	Open(userID, id string) (io.ReadCloser, error)
	// Delete removes a user's blob by id; a missing blob is not an error.
	Delete(userID, id string) error
}

// localBlobStore is the filesystem blob backend: <root>/__uploads__/<userID>/<id>.webp.
type localBlobStore struct {
	root string
}

func (l *localBlobStore) userDir(userID string) (string, error) {
	if userID == "" || strings.ContainsAny(userID, `/\`) {
		return "", fmt.Errorf("invalid user id %q", userID)
	}
	dir := filepath.Join(l.root, uploadsDir, userID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create upload dir: %w", err)
	}
	return dir, nil
}

func (l *localBlobStore) Put(userID, id string, data []byte) error {
	dir, err := l.userDir(userID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, id+userUploadSuffix), data, 0o644); err != nil {
		return fmt.Errorf("write upload blob: %w", err)
	}
	return nil
}

func (l *localBlobStore) Open(userID, id string) (io.ReadCloser, error) {
	dir, err := l.userDir(userID)
	if err != nil {
		return nil, err
	}
	abs, err := resolveWithin(dir, id+userUploadSuffix)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("open upload blob: %w", err)
	}
	return f, nil
}

func (l *localBlobStore) Delete(userID, id string) error {
	dir, err := l.userDir(userID)
	if err != nil {
		return err
	}
	abs, err := resolveWithin(dir, id+userUploadSuffix)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete upload blob: %w", err)
	}
	return nil
}

// encodeWebP decodes raw image bytes (PNG/JPEG/GIF/WebP) and re-encodes them
// as WebP. A decode failure rejects the input (fail-closed: we never store an
// unverifiable blob).
func encodeWebP(raw []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedImage, err)
	}
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, webp.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("encode webp: %w", err)
	}
	return buf.Bytes(), nil
}

// sessionDir returns the absolute directory for a session, creating it.
func (s *ImageStore) sessionDir(sessionID string) (string, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	dir := filepath.Join(s.root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create session dir: %w", err)
	}
	return dir, nil
}

// resolve confines a session-relative path to the session dir and returns the
// absolute path. It rejects empty, absolute, and escaping paths.
func resolveWithin(dir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("absolute path not allowed: %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %q", rel)
	}
	abs := filepath.Join(dir, clean)
	// Belt-and-braces: the joined path must stay under dir.
	if abs != dir && !strings.HasPrefix(abs, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %q", rel)
	}
	return abs, nil
}

// ErrUnsupportedImage reports that Save could not decode the payload as a
// supported image format (PNG/JPEG/GIF/WebP). The upload endpoint maps it to a
// 415; callers may also treat it as "reject".
var ErrUnsupportedImage = errors.New("unsupported or malformed image")

// Save decodes raw image bytes (PNG/JPEG/GIF/WebP), re-encodes to WebP, writes
// it under the session dir, and returns the session-relative path to store in a
// message block. A decode failure rejects the image (fail-closed: we never
// store an unverifiable blob).
func (s *ImageStore) Save(sessionID, name string, raw []byte) (relPath string, err error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	// Normalize the name to a safe base (strip any directory the caller passed).
	base := filepath.Base(filepath.Clean(name))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", fmt.Errorf("invalid image name %q", name)
	}
	base = strings.TrimSuffix(base, filepath.Ext(base)) + ".webp"

	enc, err := encodeWebP(raw)
	if err != nil {
		return "", err
	}

	abs, err := resolveWithin(dir, base)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, enc, 0o644); err != nil {
		return "", fmt.Errorf("write image: %w", err)
	}
	// Return the session-relative path with forward slashes (canonical form).
	return filepath.ToSlash(base), nil
}

// Open reads a session-relative image path, confined to the session dir. The
// caller streams the bytes (e.g. to base64 for the LLM, or to the HTTP
// response for the frontend).
func (s *ImageStore) Open(sessionID, relPath string) (io.ReadCloser, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return nil, err
	}
	abs, err := resolveWithin(dir, filepath.FromSlash(relPath))
	if err != nil {
		return nil, err
	}
	// Resolve symlinks and re-check confinement (defends against a planted link).
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		rd, rerr := filepath.EvalSymlinks(dir)
		if rerr == nil && resolved != rd && !strings.HasPrefix(resolved, rd+string(filepath.Separator)) {
			return nil, fmt.Errorf("path escapes workspace: %q", relPath)
		}
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("open image: %w", err)
	}
	return f, nil
}

// SaveUserUpload decodes raw image bytes and stores the WebP blob under the
// user's upload scope, returning the message-reference path ("uploads/<id>.webp")
// and the encoded byte size. It is session-independent, so a brand-new
// conversation's first message can carry an image (change user-image-uploads).
func (s *ImageStore) SaveUserUpload(userID, name string, raw []byte) (path string, size int64, err error) {
	base := filepath.Base(filepath.Clean(name))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", 0, fmt.Errorf("invalid image name %q", name)
	}
	if len(base) > maxUploadFilename {
		base = base[:maxUploadFilename]
	}

	enc, err := encodeWebP(raw)
	if err != nil {
		return "", 0, err
	}
	id := uuid.New().String()
	if err := s.blobs.Put(userID, id, enc); err != nil {
		return "", 0, err
	}
	return userUploadPrefix + id + userUploadSuffix, int64(len(enc)), nil
}

// parseUserUpload extracts the blob id from a "uploads/<id>.webp" reference,
// rejecting anything that does not match the canonical shape (so a path can
// never reach outside the user's blob scope).
func parseUserUpload(path string) (string, error) {
	if !strings.HasPrefix(path, userUploadPrefix) {
		return "", fmt.Errorf("not a user upload path: %q", path)
	}
	rest := strings.TrimPrefix(path, userUploadPrefix)
	if !strings.HasSuffix(rest, userUploadSuffix) {
		return "", fmt.Errorf("invalid upload path %q", path)
	}
	id := strings.TrimSuffix(rest, userUploadSuffix)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("invalid upload id in path %q", path)
	}
	return id, nil
}

// OpenUserUpload reads a user-level upload blob by its "uploads/<id>.webp"
// reference, confined to the user's blob scope.
func (s *ImageStore) OpenUserUpload(userID, path string) (io.ReadCloser, error) {
	id, err := parseUserUpload(path)
	if err != nil {
		return nil, err
	}
	return s.blobs.Open(userID, id)
}

// DeleteUserUpload removes a user-level upload blob. A reference path or a bare
// id is accepted; a missing blob is not an error (the record may already be gone).
func (s *ImageStore) DeleteUserUpload(userID, pathOrID string) error {
	id := pathOrID
	if strings.HasPrefix(pathOrID, userUploadPrefix) {
		var err error
		id, err = parseUserUpload(pathOrID)
		if err != nil {
			return err
		}
	}
	return s.blobs.Delete(userID, id)
}

// DeleteSessionImages removes the session's image files — every *.webp stored
// directly under <root>/<sessionID> — and, once the dir is empty, the dir
// itself. The id is validated exactly as saves validate it (no separators, no
// traversal), so a hostile id can never escape the root. Only IMAGE files are
// touched: the local sandbox backend may share the same root, so a sibling
// file or subdirectory in the session dir (a sandbox workspace) must survive.
// A missing dir is not an error.
func (s *ImageStore) DeleteSessionImages(sessionID string) error {
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, `/\`) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}
	dir := filepath.Join(s.root, sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing stored for this session
		}
		return fmt.Errorf("read session dir: %w", err)
	}
	var removed bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".webp") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("remove session image: %w", err)
		}
		removed = true
	}
	if removed {
		// Reclaim the session dir when the sweep left it empty. If non-image
		// files (a shared sandbox workspace) remain, this fails and the dir
		// stays — correct either way, and best-effort by design.
		_ = os.Remove(dir)
	}
	return nil
}

// DeleteUserUploadScope removes a user's entire upload scope — every blob
// under <root>/__uploads__/<userID> and the dir itself. The dir contains ONLY
// that user's blobs, so RemoveAll is confined by construction; the user id is
// validated like every blob write (no separators), and a missing scope is not
// an error. Called when the account row is hard-deleted, so its blobs do not
// orphan.
func (s *ImageStore) DeleteUserUploadScope(userID string) error {
	if userID == "" || strings.ContainsAny(userID, `/\`) {
		return fmt.Errorf("invalid user id %q", userID)
	}
	if err := os.RemoveAll(filepath.Join(s.root, uploadsDir, userID)); err != nil {
		return fmt.Errorf("remove user upload scope: %w", err)
	}
	return nil
}

// SweepEndedSessionImages is the retention sweep (P2-8 no-data-hard-delete):
// it deletes the image directory of every session the lister reports as ended
// before cutoff, bounded by limit per pass so one scan cannot grow unbounded.
// Only each listed session's own image dir is removed — nothing else under the
// workspace root. Best-effort per session: a failure is logged and the sweep
// continues with the next id. Returns the number of directories removed.
func SweepEndedSessionImages(ctx context.Context, log *slog.Logger, images *ImageStore, listEnded func(ctx context.Context, before time.Time, limit int) ([]string, error), cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	if images == nil {
		return 0, nil
	}
	var removed int
	for {
		ids, err := listEnded(ctx, cutoff, limit)
		if err != nil {
			return removed, fmt.Errorf("list ended sessions: %w", err)
		}
		if len(ids) == 0 {
			return removed, nil
		}
		for _, id := range ids {
			if err := images.DeleteSessionImages(id); err != nil {
				if log != nil {
					log.Warn("sweep: remove session images failed", "session", id, "err", err)
				}
				continue
			}
			removed++
		}
		if len(ids) < limit {
			return removed, nil
		}
	}
}

// ResolverFor returns a provider.ImageResolver bound to one session and its
// owner, for the pre-send materialization transform. It resolves both path
// forms: "uploads/…" references from the user's upload scope, and session-
// relative paths from that session's directory — so a run can only ever
// materialize its own session's images and its own user's uploads.
func (s *ImageStore) ResolverFor(sessionID, userID string) imageResolver {
	return imageResolver{store: s, sessionID: sessionID, userID: userID}
}

type imageResolver struct {
	store     *ImageStore
	sessionID string
	userID    string
}

// ResolveImage reads the referenced image bytes into memory, dispatching on the
// path form: user-level uploads vs session-scoped images.
func (r imageResolver) ResolveImage(_ context.Context, path string) ([]byte, error) {
	if strings.HasPrefix(path, userUploadPrefix) {
		rc, err := r.store.OpenUserUpload(r.userID, path)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	rc, err := r.store.Open(r.sessionID, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

var _ provider.ImageResolver = imageResolver{}
