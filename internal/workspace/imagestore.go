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
	"os"
	"path/filepath"
	"strings"

	"github.com/gen2brain/webp"

	"nowhere-agent/internal/provider"
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
type ImageStore struct {
	root string
}

// NewImageStore creates an ImageStore rooted at dir (created on demand).
func NewImageStore(root string) *ImageStore { return &ImageStore{root: root} }

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

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsupportedImage, err)
	}
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, webp.Options{Quality: 80}); err != nil {
		return "", fmt.Errorf("encode webp: %w", err)
	}

	abs, err := resolveWithin(dir, base)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, buf.Bytes(), 0o644); err != nil {
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

// ResolverFor returns a provider.ImageResolver bound to one session, for the
// pre-send materialization transform. Reads are confined to that session's
// directory, so a run can only ever materialize its own session's images.
func (s *ImageStore) ResolverFor(sessionID string) imageResolver {
	return imageResolver{store: s, sessionID: sessionID}
}

type imageResolver struct {
	store     *ImageStore
	sessionID string
}

// ResolveImage reads the session-relative image path into memory.
func (r imageResolver) ResolveImage(_ context.Context, path string) ([]byte, error) {
	rc, err := r.store.Open(r.sessionID, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

var _ provider.ImageResolver = imageResolver{}
