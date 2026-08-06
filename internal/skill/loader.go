package skill

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"nowhere-agent/internal/identity"
)

// manifestName is the file every skill directory must carry.
const manifestName = "SKILL.md"

// scriptExts are the file extensions treated as executable L2 scripts (run in
// the session sandbox); every other sibling file is a readable L2 resource.
var scriptExts = map[string]bool{
	".py":   true,
	".js":   true,
	".mjs":  true,
	".sh":   true,
	".bash": true,
}

// LoadDir seeds a Writer from a skills directory on disk (capability-gap K3):
// each immediate subdirectory of dir that contains a SKILL.md becomes one
// system-scope skill. The manifest supplies the L0 metadata (name, description)
// and L1 body; sibling files with a script extension become executable L2
// scripts, every other sibling file becomes an L2 resource, both keyed by path
// relative to the skill dir. Scripts run in the session sandbox via ScriptTool
// (C17 fixed: interpreter-per-extension, no sh -c concatenation).
//
// Returns the number of skills loaded. A directory that fails to parse is an
// error naming it; a missing dir is an error (config points at a real path).
func LoadDir(ctx context.Context, st Writer, dir string) (int, error) {
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
		if _, err := st.Put(ctx, *sk, "seed"); err != nil {
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
		Scripts:     map[string]string{},
	}

	// Capture sibling files as L2 scripts (executable extensions) or L2
	// resources (everything else), keyed by path relative to the skill dir;
	// the manifest itself is the body, neither script nor resource.
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
		// Normalize to forward slashes so keys are platform-stable.
		key := filepath.ToSlash(rel)
		if scriptExts[strings.ToLower(filepath.Ext(key))] {
			sk.Scripts[key] = string(data)
		} else {
			sk.Resources[key] = string(data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sk, nil
}
