package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Email case-insensitivity (migration 000046 + normalizeEmail): "User@X.com"
// and "user@x.com" must be ONE account across signup, login, and the SSO merge.

func TestSignupLoginEmailCaseInsensitive(t *testing.T) {
	db := pgTestDB(t)
	svc := NewService(NewStore(db))
	ctx := context.Background()
	email := "ci-" + randSuffix() + "@Test.dev"
	password := "pw-" + randSuffix()

	u, err := svc.Signup(ctx, "  "+email, password, "Case User")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })
	if u.Email != strings.ToLower(email) {
		t.Fatalf("stored email = %q, want %q (lowercased)", u.Email, strings.ToLower(email))
	}

	// The same address in different casing is the same account, not a second one.
	if _, err := svc.Signup(ctx, strings.ToUpper(email), "another-pw-1", "x"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("case-variant signup = %v, want ErrUserExists", err)
	}

	// Login matches regardless of input casing and whitespace.
	token, got, err := svc.Login(ctx, " "+strings.ToUpper(email)+" ", password)
	if err != nil {
		t.Fatalf("login with mixed-case email: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("login resolved user %q, want %q", got.ID, u.ID)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM auth_tokens WHERE token_hash = $1`, hashToken(token)) })

	// LookupByEmail agrees.
	found, err := svc.LookupByEmail(ctx, strings.ToUpper(email))
	if err != nil || found.ID != u.ID {
		t.Fatalf("LookupByEmail = %v (id %q), want user %q", err, found.ID, u.ID)
	}
}

func TestProvisionExternalUserEmailCaseInsensitiveMerge(t *testing.T) {
	db := pgTestDB(t)
	svc := NewService(NewStore(db))
	store := NewStore(db)
	ctx := context.Background()
	email := "sso-ci-" + randSuffix() + "@Corp.test"
	issuer := "https://idp-" + randSuffix() + ".test"
	subject := "sub-" + randSuffix()

	passwordUser, err := svc.Signup(ctx, email, "pw-"+randSuffix(), "Password User")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, passwordUser.ID) })

	// The IdP asserts the same address with different casing: the SSO sign-in
	// must join the password account, not create a second one.
	u, err := store.ProvisionExternalUser(ctx, issuer, subject, strings.ToUpper(email), "SSO User")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if u.ID != passwordUser.ID {
		t.Fatalf("SSO provision resolved user %q, want the password account %q", u.ID, passwordUser.ID)
	}
	if u.Email != strings.ToLower(email) {
		t.Fatalf("provisioned email = %q, want %q", u.Email, strings.ToLower(email))
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM user_identities WHERE user_id = $1`, u.ID) })
}
