package memory

import (
	"context"
	"errors"
	"testing"

	"nowhere-agent/internal/identity"
)

// GetByID is the scope check that guards Deprecate and Forget, which take a
// bare id. Both implementations must agree on it, or the check silently
// weakens depending on which port is wired.

func TestMemPortGetByID(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()

	stored, err := p.Store(ctx, Memory{
		Scope:   identity.TeamScope("team-1"),
		Kind:    KindFact,
		Content: "the deploy key rotates on Fridays",
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := p.GetByID(ctx, stored.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != stored.ID {
		t.Errorf("id = %q, want %q", got.ID, stored.ID)
	}
	// The scope is the whole point — an authorization check reading a zero
	// scope would treat a team memory as system-scoped.
	if got.Scope.Scope != identity.ScopeTeam || got.Scope.TeamID != "team-1" {
		t.Errorf("scope = %+v, want team scope of team-1", got.Scope)
	}
	if got.Content != stored.Content {
		t.Errorf("content = %q, want %q", got.Content, stored.Content)
	}
}

func TestMemPortGetByIDNotFound(t *testing.T) {
	p := NewMemPort()
	if _, err := p.GetByID(context.Background(), "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMemPortGetByIDAfterForget(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	m, err := p.Store(ctx, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Forget(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetByID(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after Forget", err)
	}
}

// A deprecated memory is excluded from recall but still exists, so GetByID must
// still find it — otherwise "un-deprecate" or an audit could never reach it.
func TestMemPortGetByIDFindsDeprecated(t *testing.T) {
	p := NewMemPort()
	ctx := context.Background()
	m, err := p.Store(ctx, Memory{Scope: identity.UserScope("u1"), Kind: KindFact, Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Deprecate(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetByID on a deprecated memory: %v", err)
	}
	if !got.Deprecated {
		t.Error("memory should report as deprecated")
	}
}

func TestPGPortGetByID(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()
	scope := identity.TeamScope("team-getbyid")

	m, err := p.Store(ctx, Memory{Scope: scope, Kind: KindPreference, Content: "prefers short summaries"})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	cleanup(t, db, m.ID)

	got, err := p.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != m.ID || got.Content != m.Content {
		t.Errorf("GetByID = %+v, want id %s content %q", got, m.ID, m.Content)
	}
	// The scope round-trips, so an authorization check can rely on it.
	if got.Scope.Scope != identity.ScopeTeam || got.Scope.TeamID != "team-getbyid" {
		t.Errorf("scope = %+v, want team scope of team-getbyid", got.Scope)
	}
}

func TestPGPortGetByIDNotFound(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	_, err := p.GetByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPGPortGetByIDFindsDeprecated(t *testing.T) {
	db := pgTestDB(t)
	p := NewPGPort(db)
	ctx := context.Background()

	m, err := p.Store(ctx, Memory{Scope: identity.SystemScope(), Kind: KindFact, Content: "deprecated but present"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(t, db, m.ID)
	if err := p.Deprecate(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetByID on a deprecated memory: %v", err)
	}
	if !got.Deprecated {
		t.Error("memory should report as deprecated")
	}
}
