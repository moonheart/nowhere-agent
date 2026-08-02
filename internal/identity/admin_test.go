package identity

import (
	"context"
	"errors"
	"testing"
)

// The self-target guards short-circuit before touching the store, so they are
// exercised against a Service with no database at all — if one of them ever
// starts hitting the store first, these tests panic rather than silently
// passing.

func TestSetPlatformRoleRefusesSelfDemotion(t *testing.T) {
	svc := NewService(nil)
	err := svc.SetPlatformRole(context.Background(), "u1", "u1", PlatformRoleUser)
	if !errors.Is(err, ErrSelfTarget) {
		t.Fatalf("err = %v, want ErrSelfTarget", err)
	}
}

func TestSetPlatformRoleAllowsSelfPromotion(t *testing.T) {
	// Re-granting yourself the role you already hold changes nothing and cannot
	// lock anyone out, so it is not worth refusing. It must reach the store,
	// which is nil here — a panic proves it got past the guard.
	svc := NewService(nil)
	defer func() {
		if recover() == nil {
			t.Error("self-promotion should have reached the store, not been refused")
		}
	}()
	_ = svc.SetPlatformRole(context.Background(), "u1", "u1", PlatformRoleAdmin)
}

func TestSetPlatformRoleRejectsUnknownRole(t *testing.T) {
	svc := NewService(nil)
	if err := svc.SetPlatformRole(context.Background(), "actor", "u1", PlatformRole("superuser")); err == nil {
		t.Fatal("expected an unknown platform role to be rejected")
	}
}

func TestSetUserDisabledRefusesSelfDisable(t *testing.T) {
	svc := NewService(nil)
	err := svc.SetUserDisabled(context.Background(), "u1", "u1", true)
	if !errors.Is(err, ErrSelfTarget) {
		t.Fatalf("err = %v, want ErrSelfTarget", err)
	}
}

func TestSetUserDisabledAllowsSelfEnable(t *testing.T) {
	// Re-enabling yourself is a no-op for a caller who is, by definition,
	// authenticated — no lock-out is possible, so it passes through.
	svc := NewService(nil)
	defer func() {
		if recover() == nil {
			t.Error("self re-enable should have reached the store")
		}
	}()
	_ = svc.SetUserDisabled(context.Background(), "u1", "u1", false)
}

func TestDeleteAccountRefusesSelf(t *testing.T) {
	svc := NewService(nil)
	err := svc.DeleteAccount(context.Background(), "u1", "u1")
	if !errors.Is(err, ErrSelfTarget) {
		t.Fatalf("err = %v, want ErrSelfTarget", err)
	}
}

func TestPromoteByEmailIgnoresEmptyEmail(t *testing.T) {
	// An unset BOOTSTRAP_ADMIN_EMAIL must not reach the database, or every
	// startup without one would error.
	svc := NewService(nil)
	found, err := svc.PromoteByEmail(context.Background(), "")
	if err != nil || found {
		t.Fatalf("PromoteByEmail(\"\") = %v, %v; want false, nil", found, err)
	}
}

func TestAddMemberByEmailRejectsInvalidRole(t *testing.T) {
	svc := NewService(nil)
	if _, err := svc.AddMemberByEmail(context.Background(), "team", "a@b.c", Role("superuser")); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("err = %v, want ErrInvalidRole", err)
	}
}

func TestRoleRank(t *testing.T) {
	cases := []struct {
		role Role
		min  Role
		want bool
	}{
		{RoleOwner, RoleOwner, true},
		{RoleOwner, RoleAdmin, true},
		{RoleOwner, RoleMember, true},
		{RoleAdmin, RoleOwner, false},
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleMember, true},
		{RoleMember, RoleAdmin, false},
		{RoleMember, RoleMember, true},
		{Role("bogus"), RoleMember, false},
		{Role(""), RoleMember, false},
	}
	for _, c := range cases {
		if got := c.role.AtLeast(c.min); got != c.want {
			t.Errorf("Role(%q).AtLeast(%q) = %v, want %v", c.role, c.min, got, c.want)
		}
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleOwner, RoleAdmin, RoleMember} {
		if !r.Valid() {
			t.Errorf("Role(%q) should be valid", r)
		}
	}
	for _, r := range []Role{"", "superuser", "Owner"} {
		if Role(r).Valid() {
			t.Errorf("Role(%q) should not be valid", r)
		}
	}
}

func TestUserIsAdminAndDisabled(t *testing.T) {
	if (User{PlatformRole: PlatformRoleAdmin}).IsAdmin() != true {
		t.Error("admin role should report IsAdmin")
	}
	if (User{PlatformRole: PlatformRoleUser}).IsAdmin() != false {
		t.Error("user role should not report IsAdmin")
	}
	// The zero value is an ordinary, enabled account — a struct built without
	// touching the database must never accidentally read as an administrator.
	var zero User
	if zero.IsAdmin() || zero.Disabled() {
		t.Error("zero User should be a non-admin, enabled account")
	}
}

func TestIsNotFound(t *testing.T) {
	for _, err := range []error{ErrUserNotFound, ErrTeamNotFound, ErrNotMember} {
		if !IsNotFound(err) {
			t.Errorf("IsNotFound(%v) = false, want true", err)
		}
	}
	for _, err := range []error{nil, ErrInvalidCredentials, ErrLastOwner, ErrSelfTarget} {
		if IsNotFound(err) {
			t.Errorf("IsNotFound(%v) = true, want false", err)
		}
	}
}
