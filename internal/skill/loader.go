package skill

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"nowhere-agent/internal/identity"
)

// manifestName is the file every skill directory must carry.
const manifestName = "SKILL.md"

// LoadDir seeds a Store from a skills directory on disk (capability-gap K3a):
// each immediate subdirectory of dir that contains a SKILL.md becomes one
// system-scope skill. The manifest supplies the L0 metadata (name, description)
// and L1 body; every other file in the directory is captured as an L2 resource
// by relative path. Scripts are intentionally NOT loaded — skill script
// execution is deferred to K3b pending the C17 exec-safety fix, so this loader
// only ever produces readable (never executable) skills.
//
// Returns the number of skills loaded. A directory that fails to parse is an
// error naming it; a missing dir is an error (config points at a real path).
func LoadDir(ctx context.Context, st *Store, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read skills dir: %w", err)
	}
	loaded := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sk, err := loadSkillDir(filepath.Join(dir, e.Name()))
		if err != nil {
			return loaded, fmt.Errorf("skill %q: %w", e.Name(), err)
		}
		if sk == nil {
			continue // no SKILL.md in this subdirectory — not a skill
		}
		if _, err := st.Put(ctx, *sk); err != nil {
			return loaded, fmt.Errorf("store skill %q: %w", sk.Name, err)
		}
		loaded++
	}
	return loaded, nil
}

// loadSkillDir reads one skill directory into a system-scope Skill, or returns
// nil if the directory holds no SKILL.md.
func loadSkillDir(dir string) (*Skill, error) {
	manifestPath := filepath.Join(dir, manifestName)
	doc, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	m, err := ParseManifest(string(doc))
	if err != nil {
		return nil, err
	}

	sk := &Skill{
		Name:        m.Name,
		Description: m.Description,
		Body:        m.Body,
		Scope:       identity.SystemScope(),
		Resources:   map[string]string{},
		Scripts:     map[string]string{}, // deliberately empty until K3b
	}

	// Capture sibling files as L2 resources (keyed by path relative to the
	// skill dir); the manifest itself is the body, not a resource.
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == manifestName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Normalize to forward slashes so resource keys are platform-stable.
		sk.Resources[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sk, nil
}
