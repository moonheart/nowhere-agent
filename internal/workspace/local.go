package workspace

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// LocalStore is the built-in Store backed by a local directory. Layout:
//
//	<root>/<sessionID>/versions/<n>/...   snapshot contents
//	<root>/<sessionID>/staging/...        in-progress solidify
//	<root>/<sessionID>/current            JSON pointer to committed version
//
// Solidify is atomic: content is written to staging, then renamed to a version
// dir, then the current pointer is updated. An interruption before the pointer
// update leaves the prior committed version intact.
type LocalStore struct {
	root string
	now  func() time.Time
}

// NewLocalStore creates a LocalStore rooted at dir.
func NewLocalStore(root string) (*LocalStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace root: %w", err)
	}
	return &LocalStore{root: root, now: time.Now}, nil
}

func (s *LocalStore) sessionDir(sessionID string) string {
	return filepath.Join(s.root, sessionID)
}

func (s *LocalStore) currentPath(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), "current")
}

func (s *LocalStore) versionsDir(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), "versions")
}

func (s *LocalStore) stagingDir(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), "staging")
}

// Current returns the latest committed version, or false if none.
func (s *LocalStore) Current(sessionID string) (Ref, bool) {
	b, err := os.ReadFile(s.currentPath(sessionID))
	if err != nil {
		return Ref{}, false
	}
	var p pointer
	if err := json.Unmarshal(b, &p); err != nil {
		return Ref{}, false
	}
	return Ref{SessionID: sessionID, Version: p.Version}, true
}

type pointer struct {
	Version int64 `json:"version"`
}

// Solidify persists dir as a new version atomically.
func (s *LocalStore) Solidify(sessionID, dir string) (Ref, error) {
	if err := os.MkdirAll(s.sessionDir(sessionID), 0o755); err != nil {
		return Ref{}, err
	}

	// 1. Write incoming content into a fresh staging dir.
	staging := s.stagingDir(sessionID)
	if err := os.RemoveAll(staging); err != nil {
		return Ref{}, err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return Ref{}, err
	}
	if err := copyDir(dir, staging); err != nil {
		return Ref{}, fmt.Errorf("copy to staging: %w", err)
	}

	// 2. Determine next version and move staging into place.
	next := int64(1)
	if cur, ok := s.Current(sessionID); ok {
		next = cur.Version + 1
	}
	versionDir := filepath.Join(s.versionsDir(sessionID), strconv.FormatInt(next, 10))
	if err := os.MkdirAll(s.versionsDir(sessionID), 0o755); err != nil {
		return Ref{}, err
	}
	if err := os.Rename(staging, versionDir); err != nil {
		return Ref{}, fmt.Errorf("promote staging: %w", err)
	}

	// 3. Atomically update the current pointer (write temp + rename). If the
	// pointer cannot be committed, roll the promoted version dir back: the
	// version counter derives from the pointer, so without a rollback the next
	// solidify would rename onto the existing dir (fails on Windows, silently
	// replaces on Unix).
	p := pointer{Version: next}
	b, err := json.Marshal(p)
	if err != nil {
		os.RemoveAll(versionDir)
		return Ref{}, err
	}
	tmp := s.currentPath(sessionID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		os.RemoveAll(versionDir)
		return Ref{}, err
	}
	if err := os.Rename(tmp, s.currentPath(sessionID)); err != nil {
		os.RemoveAll(versionDir)
		return Ref{}, fmt.Errorf("commit pointer: %w", err)
	}

	return Ref{SessionID: sessionID, Version: next}, nil
}

// Materialize extracts the current version into dir.
func (s *LocalStore) Materialize(sessionID, dir string) (Ref, error) {
	ref, ok := s.Current(sessionID)
	if !ok {
		// No persisted workspace yet; ensure dir exists and return zero ref.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Ref{}, err
		}
		return Ref{SessionID: sessionID, Version: 0}, nil
	}
	src := filepath.Join(s.versionsDir(sessionID), strconv.FormatInt(ref.Version, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Ref{}, err
	}
	if err := copyDir(src, dir); err != nil {
		return Ref{}, fmt.Errorf("materialize: %w", err)
	}
	return ref, nil
}

// Delete removes all versions and metadata for a session.
func (s *LocalStore) Delete(sessionID string) error {
	return os.RemoveAll(s.sessionDir(sessionID))
}

// Meta returns stats about a version (helper for inspection/tests).
func (s *LocalStore) Meta(ref Ref) (Meta, error) {
	dir := filepath.Join(s.versionsDir(ref.SessionID), strconv.FormatInt(ref.Version, 10))
	m := Meta{Ref: ref, CreatedAt: s.now()}
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			m.Files++
			if fi, err := d.Info(); err == nil {
				m.Bytes += fi.Size()
			}
		}
		return nil
	})
	return m, err
}

// copyDir recursively copies src into dst (dst must exist).
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// listFiles returns the relative paths of all files under dir (sorted).
func listFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}
