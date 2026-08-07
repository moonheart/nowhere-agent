// Package skill implements the skill-system capability (design D7/D16):
// general-form skills (SKILL.md + resources + scripts) with progressive
// disclosure across system/team/user scopes, versioned with override review.
// Skill L2 scripts execute in the session sandbox via the tool runtime.
package skill

import (
	"sort"
	"time"

	"nowhere-agent/internal/identity"
)

// Skill is a general-form capability package.
type Skill struct {
	ID      string
	Name    string
	Scope   identity.ScopeRef
	Version int
	// Description is the L0 one-liner shown in context.
	Description string
	// Body is the L1 full SKILL.md instructions, loaded when selected.
	Body string
	// Resources are L2 files referenced by the body, loaded on demand.
	Resources map[string]string
	// Scripts are L2 executable entry points run in the sandbox.
	Scripts map[string]string
	// OverridesVersion records which version of a lower-scope skill this
	// override was based on (for review when upstream updates).
	OverridesVersion int
	// NeedsReview is set when the upstream skill this overrides was updated.
	NeedsReview bool
	// Enabled gates agent resolution only: a disabled skill drops out of Get/List
	// (so the model stops seeing it) but stays visible and editable in the
	// management surface. Disabling is the reversible alternative to Delete.
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// L0 is the always-resident metadata view of a skill.
type L0 struct {
	Name        string
	Description string
	// Scripts are the names of the skill's executable entry points (the keys of
	// Skill.Scripts), surfaced so the model can discover what run_skill_script can
	// run without loading the full L1 body. Nil when the skill has no scripts.
	Scripts []string
}

// Manifest is the parsed SKILL.md frontmatter + body.
type Manifest struct {
	Name        string
	Description string
	Body        string
}

// scriptNames returns the sorted keys of a skill's Scripts map, so L0 listings
// and the run_skill_script tool see a deterministic order.
func scriptNames(scripts map[string]string) []string {
	if len(scripts) == 0 {
		return nil
	}
	out := make([]string, 0, len(scripts))
	for name := range scripts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
