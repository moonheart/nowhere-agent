package skill

import (
	"context"
	"testing"

	"nowhere-agent/internal/identity"
)

func ctx() context.Context { return context.Background() }

func TestPutAssignsVersion(t *testing.T) {
	s := NewStore()
	sk, err := s.Put(ctx(), Skill{Name: "deploy", Scope: identity.SystemScope(), Description: "d", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if sk.Version != 1 || sk.ID == "" {
		t.Errorf("version=%d id=%q", sk.Version, sk.ID)
	}
}

func TestPutUpdateIncrementsVersion(t *testing.T) {
	s := NewStore()
	s.Put(ctx(), Skill{Name: "deploy", Scope: identity.SystemScope(), Body: "v1"})
	sk, _ := s.Put(ctx(), Skill{Name: "deploy", Scope: identity.SystemScope(), Body: "v2"})
	if sk.Version != 2 {
		t.Errorf("version = %d want 2", sk.Version)
	}
	if sk.Body != "v2" {
		t.Errorf("body = %q", sk.Body)
	}
}

func TestGetScopePriorityUserOverTeam(t *testing.T) {
	s := NewStore()
	s.Put(ctx(), Skill{Name: "x", Scope: identity.TeamScope("t1"), Body: "team"})
	s.Put(ctx(), Skill{Name: "x", Scope: identity.UserScope("u1"), Body: "user"})

	scopes := []identity.ScopeRef{identity.UserScope("u1"), identity.TeamScope("t1"), identity.SystemScope()}
	got, ok := s.Get(ctx(), "x", scopes)
	if !ok {
		t.Fatal("not found")
	}
	if got.Body != "user" {
		t.Errorf("user scope should win, got %q", got.Body)
	}
}

func TestGetFallsBackToLowerScope(t *testing.T) {
	s := NewStore()
	s.Put(ctx(), Skill{Name: "x", Scope: identity.SystemScope(), Body: "system"})
	scopes := []identity.ScopeRef{identity.UserScope("u1"), identity.SystemScope()}
	got, ok := s.Get(ctx(), "x", scopes)
	if !ok || got.Body != "system" {
		t.Errorf("fallback to system failed: %+v", got)
	}
}

func TestListDeduplicatesByNameWithPriority(t *testing.T) {
	s := NewStore()
	s.Put(ctx(), Skill{Name: "a", Scope: identity.SystemScope(), Description: "sys-a"})
	s.Put(ctx(), Skill{Name: "a", Scope: identity.UserScope("u1"), Description: "user-a"})
	s.Put(ctx(), Skill{Name: "b", Scope: identity.TeamScope("t1"), Description: "team-b"})

	scopes := []identity.ScopeRef{identity.UserScope("u1"), identity.TeamScope("t1"), identity.SystemScope()}
	list := s.List(ctx(), scopes)
	if len(list) != 2 {
		t.Fatalf("expected 2 unique names, got %d: %+v", len(list), list)
	}
	// "a" resolved to user version.
	for _, l := range list {
		if l.Name == "a" && l.Description != "user-a" {
			t.Errorf("a = %q want user-a", l.Description)
		}
	}
}

func TestUpstreamUpdateFlagsOverrideForReview(t *testing.T) {
	s := NewStore()
	// team skill v1
	team, _ := s.Put(ctx(), Skill{Name: "deploy", Scope: identity.TeamScope("t1"), Body: "v1"})
	// user overrides it, based on v1
	user, _ := s.Put(ctx(), Skill{Name: "deploy", Scope: identity.UserScope("u1"), Body: "mine", OverridesVersion: team.Version})
	if user.NeedsReview {
		t.Error("should not need review yet")
	}
	// team skill updated to v2 → user's override flagged
	s.Put(ctx(), Skill{Name: "deploy", Scope: identity.TeamScope("t1"), Body: "v2"})

	got, ok := s.Get(ctx(), "deploy", []identity.ScopeRef{identity.UserScope("u1"), identity.TeamScope("t1")})
	if !ok {
		t.Fatal("not found")
	}
	if !got.NeedsReview {
		t.Error("override should be flagged for review after upstream update")
	}
}

func TestDelete(t *testing.T) {
	s := NewStore()
	sk, _ := s.Put(ctx(), Skill{Name: "x", Scope: identity.SystemScope()})
	s.Delete(ctx(), sk.ID)
	if _, ok := s.Get(ctx(), "x", []identity.ScopeRef{identity.SystemScope()}); ok {
		t.Error("expected skill deleted")
	}
}
