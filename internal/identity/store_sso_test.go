package identity

import (
	"context"
	"errors"
	"testing"
)

// SSO provisioning tests (enterprise-readiness P1-2). They run against the real
// dev Postgres with unique random issuer/subject pairs and clean up only the
// rows they create (the user row's ON DELETE CASCADE drops the link). user_id
// deletions are scoped to the account each test provisions.

func TestProvisionExternalUserCreatesAccountAndLink(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	issuer := "https://idp-" + randSuffix() + ".test"
	subject := "sub-" + randSuffix()
	email := "sso-" + randSuffix() + "@corp.test"

	u, err := s.ProvisionExternalUser(ctx, issuer, subject, email, "SSO User")
	if err != nil {
		t.Fatalf("ProvisionExternalUser: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })

	if u.Email != email || u.DisplayName != "SSO User" {
		t.Fatalf("provisioned account mismatch: %+v", u)
	}
	if u.PasswordHash == "" || u.PasswordHash == "x" {
		t.Fatalf("sso account should carry the unusable-password sentinel, got %q", u.PasswordHash)
	}

	// Resolvable by the external identity.
	got, err := s.UserByExternalIdentity(ctx, issuer, subject)
	if err != nil {
		t.Fatalf("UserByExternalIdentity: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("resolved %q, want %q", got.ID, u.ID)
	}
}

func TestProvisionExternalUserIsIdempotent(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	issuer := "https://idp-" + randSuffix() + ".test"
	subject := "sub-" + randSuffix()
	email := "sso-" + randSuffix() + "@corp.test"

	first, err := s.ProvisionExternalUser(ctx, issuer, subject, email, "First")
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, first.ID) })

	// A second sign-in for the same identity must return the SAME account, not
	// create a duplicate.
	second, err := s.ProvisionExternalUser(ctx, issuer, subject, email, "First")
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent provision returned %q, want %q", second.ID, first.ID)
	}
}

func TestProvisionExternalUserJoinsExistingEmailAccount(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()

	// An account that already exists (created via password signup path, which the
	// mkUser helper emulates with a real password_hash).
	existing := mkUser(t, db)
	issuer := "https://idp-" + randSuffix() + ".test"
	subject := "sub-" + randSuffix()

	// SSO sign-in asserting the SAME email must link to that account, so an
	// employee who first used a password and later uses SSO keeps one account.
	u, err := s.ProvisionExternalUser(ctx, issuer, subject, existing.Email, "Whatever")
	if err != nil {
		t.Fatalf("provision onto existing email: %v", err)
	}
	if u.ID != existing.ID {
		t.Fatalf("joined %q, want the existing account %q", u.ID, existing.ID)
	}

	// And the link resolves.
	got, err := s.UserByExternalIdentity(ctx, issuer, subject)
	if err != nil || got.ID != existing.ID {
		t.Fatalf("resolve after join: id=%q err=%v", got.ID, err)
	}
}

func TestUserByExternalIdentityNotFound(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	_, err := s.UserByExternalIdentity(context.Background(),
		"https://idp-"+randSuffix()+".test", "sub-"+randSuffix())
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown identity should be ErrUserNotFound, got %v", err)
	}
}

func TestProvisionExternalUserDistinctSubjectsDistinctAccounts(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	issuer := "https://idp-" + randSuffix() + ".test"
	email := "sso-" + randSuffix() + "@corp.test"

	// Two different external subjects at the SAME IdP asserting the same email:
	// the first creates the account; the second joins it by email (the enterprise
	// email is the join key), so both identities resolve to one account.
	a, err := s.ProvisionExternalUser(ctx, issuer, "sub-"+randSuffix(), email, "A")
	if err != nil {
		t.Fatalf("provision a: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, a.ID) })
	b, err := s.ProvisionExternalUser(ctx, issuer, "sub-"+randSuffix(), email, "B")
	if err != nil {
		t.Fatalf("provision b: %v", err)
	}
	if b.ID != a.ID {
		t.Fatalf("same-email subjects should share one account, got a=%q b=%q", a.ID, b.ID)
	}
}
