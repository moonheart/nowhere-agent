package agentdef

import (
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
)

func TestParse(t *testing.T) {
	doc := `---
name: reviewer
description: Reviews code for safety
tools: read_file, list_dir
disallowedTools: write_file
model: opus
maxTurns: 8
skills: lint, sast
---
You are a careful reviewer.
Report findings.`

	d, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Name != "reviewer" || d.WhenToUse != "Reviews code for safety" {
		t.Fatalf("name/desc: %q / %q", d.Name, d.WhenToUse)
	}
	if len(d.Tools) != 2 || d.Tools[0] != "read_file" || d.Tools[1] != "list_dir" {
		t.Fatalf("tools: %v", d.Tools)
	}
	if len(d.DisallowedTools) != 1 || d.DisallowedTools[0] != "write_file" {
		t.Fatalf("disallowed: %v", d.DisallowedTools)
	}
	if d.Model != "opus" || d.MaxTurns != 8 {
		t.Fatalf("model/maxTurns: %q / %d", d.Model, d.MaxTurns)
	}
	if len(d.Skills) != 2 || d.Skills[0] != "lint" {
		t.Fatalf("skills: %v", d.Skills)
	}
	if !strings.HasPrefix(d.System, "You are a careful reviewer.") || !strings.Contains(d.System, "Report findings.") {
		t.Fatalf("system body: %q", d.System)
	}
	if d.Wildcard() {
		t.Fatalf("explicit tool list should not be wildcard")
	}
}

func TestParseWildcardAndErrors(t *testing.T) {
	d, err := Parse("---\nname: x\ndescription: y\n---\nbody")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !d.Wildcard() {
		t.Fatalf("omitted tools should be wildcard")
	}

	if _, err := Parse("no frontmatter here"); err == nil {
		t.Fatalf("expected error for missing frontmatter")
	}
	if _, err := Parse("---\ndescription: y\n---\nbody"); err == nil {
		t.Fatalf("expected error for missing name")
	}
	if _, err := Parse("---\nname: x\n---\nbody"); err == nil {
		t.Fatalf("expected error for missing description")
	}
}

func TestResolveBuiltinDefault(t *testing.T) {
	s := NewStore()
	sys := []identity.ScopeRef{identity.SystemScope()}
	d, err := s.Resolve(GeneralPurpose, sys)
	if err != nil {
		t.Fatalf("resolve general-purpose: %v", err)
	}
	if d.Name != GeneralPurpose || !d.Wildcard() {
		t.Fatalf("unexpected builtin: %+v", d)
	}
}

func TestBuiltinsForLang(t *testing.T) {
	en := BuiltinsForLang("en")
	zh := BuiltinsForLang("zh")
	unknown := BuiltinsForLang("fr") // any other value falls back to English

	if en[0].System != unknown[0].System {
		t.Fatal("unknown lang must fall back to English")
	}
	if zh[0].System == en[0].System {
		t.Fatal("zh and en builtin prompts must differ")
	}
	// The zh builtin actually reads as Chinese.
	if !strings.Contains(zh[0].System, "子代理") {
		t.Fatalf("zh prompt not Chinese: %q", zh[0].System)
	}
	if strings.Contains(en[0].System, "子代理") {
		t.Fatalf("en prompt contains Chinese: %q", en[0].System)
	}
	// Builtins() stays the English default for existing callers.
	if got := Builtins(); got[0].System != en[0].System {
		t.Fatal("Builtins() must keep the English prompt")
	}
}

func TestStoreLangSelectsBuiltinLanguage(t *testing.T) {
	zh := NewStore("zh")
	sys := []identity.ScopeRef{identity.SystemScope()}
	d, err := zh.Resolve(GeneralPurpose, sys)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(d.System, "子代理") {
		t.Fatalf("zh store resolved an English builtin: %q", d.System)
	}
	// NewStore() with no argument stays English.
	en := NewStore()
	d2, _ := en.Resolve(GeneralPurpose, sys)
	if strings.Contains(d2.System, "子代理") {
		t.Fatalf("default store resolved a Chinese builtin: %q", d2.System)
	}
}

func TestResolveScopeOverride(t *testing.T) {
	s := NewStore()
	// A user-scoped override of general-purpose should win over the builtin.
	s.Put(AgentDef{Name: GeneralPurpose, WhenToUse: "override", System: "overridden", Scope: identity.UserScope("u1")})

	// Priority order high→low.
	scopes := []identity.ScopeRef{identity.UserScope("u1"), identity.SystemScope()}
	d, err := s.Resolve(GeneralPurpose, scopes)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.System != "overridden" {
		t.Fatalf("expected user override, got system: %q", d.System)
	}

	// A different user does not see u1's override.
	other := []identity.ScopeRef{identity.UserScope("u2"), identity.SystemScope()}
	d2, err := s.Resolve(GeneralPurpose, other)
	if err != nil {
		t.Fatalf("resolve other: %v", err)
	}
	if d2.System == "overridden" {
		t.Fatalf("u2 must not see u1's override")
	}
}

func TestResolveNormalizedAndUnknown(t *testing.T) {
	s := NewStore()
	sys := []identity.ScopeRef{identity.SystemScope()}
	s.Put(AgentDef{Name: "code-reviewer", WhenToUse: "d", Scope: identity.SystemScope()})

	for _, req := range []string{"code-reviewer", "CodeReviewer", "code_reviewer", "Code Reviewer"} {
		d, err := s.Resolve(req, sys)
		if err != nil {
			t.Fatalf("resolve %q: %v", req, err)
		}
		if d.Name != "code-reviewer" {
			t.Fatalf("resolve %q → %q", req, d.Name)
		}
	}

	_, err := s.Resolve("nope", sys)
	if err == nil || !strings.Contains(err.Error(), "available:") {
		t.Fatalf("expected unknown-with-available error, got %v", err)
	}
}

func TestResolveAmbiguous(t *testing.T) {
	s := NewStore()
	sys := []identity.ScopeRef{identity.SystemScope()}
	s.Put(AgentDef{Name: "code-reviewer", WhenToUse: "d", Scope: identity.SystemScope()})
	s.Put(AgentDef{Name: "codereviewer", WhenToUse: "d", Scope: identity.SystemScope()})

	_, err := s.Resolve("Code Reviewer", sys)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous error, got %v", err)
	}
}
