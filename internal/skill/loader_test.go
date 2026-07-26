package skill

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nowhere-agent/internal/identity"
)

// writeSkill scaffolds a skill directory under root with a SKILL.md and any
// extra files (path relative to the skill dir).
func writeSkill(t *testing.T, root, dirName, manifest string, extra map[string]string) {
	t.Helper()
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for rel, content := range extra {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLoadDirSeedsSystemSkills: each subdir with a SKILL.md becomes a
// system-scope skill — L0 metadata + L1 body + L2 resources, and no scripts
// (script loading is deferred to K3b).
func TestLoadDirSeedsSystemSkills(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "review", "---\nname: review\ndescription: Code review helper\n---\nReview the diff carefully.", map[string]string{
		"checklist.md":  "- tests\n- lint",
		"ref/rubric.md": "severity ladder",
	})
	writeSkill(t, root, "greeter", "---\nname: greeter\ndescription: Says hi\n---\nBe warm.", nil)

	st := NewStore()
	n, err := LoadDir(ctx, st, root)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if n != 2 {
		t.Errorf("loaded = %d want 2", n)
	}

	scopes := []identity.ScopeRef{identity.SystemScope()}
	l0 := st.List(ctx, scopes)
	if len(l0) != 2 {
		t.Fatalf("L0 list = %+v want 2 skills", l0)
	}

	review, ok := st.Get(ctx, "review", scopes)
	if !ok {
		t.Fatal("review skill not resolvable at system scope")
	}
	if review.Description != "Code review helper" {
		t.Errorf("description = %q", review.Description)
	}
	if review.Body != "Review the diff carefully." {
		t.Errorf("body = %q", review.Body)
	}
	if review.Resources["checklist.md"] != "- tests\n- lint" {
		t.Errorf("checklist resource = %q", review.Resources["checklist.md"])
	}
	if review.Resources["ref/rubric.md"] != "severity ladder" {
		t.Errorf("nested resource = %q", review.Resources["ref/rubric.md"])
	}
	if len(review.Scripts) != 0 {
		t.Errorf("scripts must stay empty until K3b, got %v", review.Scripts)
	}
}

// TestLoadDirSkipsNonSkillDirs: a subdirectory without SKILL.md is ignored, and
// files sitting at the root are ignored too.
func TestLoadDirSkipsNonSkillDirs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "one", "---\nname: one\ndescription: d\n---\nbody", nil)

	st := NewStore()
	n, err := LoadDir(ctx, st, root)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if n != 1 {
		t.Errorf("loaded = %d want 1 (only the dir with SKILL.md)", n)
	}
}

// TestLoadDirBadManifestFails: a skill whose SKILL.md has no name is an error
// that names the offending directory.
func TestLoadDirBadManifestFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "broken", "---\ndescription: no name here\n---\nbody", nil)

	st := NewStore()
	if _, err := LoadDir(ctx, st, root); err == nil {
		t.Fatal("expected an error for a manifest without a name")
	}
}

// TestLoadDirMissingDirFails: pointing at a nonexistent dir is an error.
func TestLoadDirMissingDirFails(t *testing.T) {
	if _, err := LoadDir(context.Background(), NewStore(), filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a missing skills dir")
	}
}
