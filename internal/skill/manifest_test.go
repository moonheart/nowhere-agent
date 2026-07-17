package skill

import "testing"

func TestParseManifestWithFrontmatter(t *testing.T) {
	doc := `---
name: deploy
description: Deploy the app
---
# Deploy

Steps to deploy.`
	m, err := ParseManifest(doc)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "deploy" {
		t.Errorf("name = %q", m.Name)
	}
	if m.Description != "Deploy the app" {
		t.Errorf("description = %q", m.Description)
	}
	if m.Body == "" || m.Body[:1] != "#" {
		t.Errorf("body = %q", m.Body)
	}
}

func TestParseManifestNoFrontmatter(t *testing.T) {
	m, err := ParseManifest("# Just a body")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "# Just a body" {
		t.Errorf("body = %q", m.Body)
	}
}

func TestParseManifestUnterminated(t *testing.T) {
	_, err := ParseManifest("---\nname: x\nno closing")
	if err == nil {
		t.Error("expected error for unterminated frontmatter")
	}
}

func TestParseManifestRequiresName(t *testing.T) {
	_, err := ParseManifest("---\ndescription: no name\n---\nbody")
	if err == nil {
		t.Error("expected error for missing name")
	}
}
