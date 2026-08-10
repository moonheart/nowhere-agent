package identity

import (
	"context"
	"errors"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	if err := validatePassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("7 chars = %v, want ErrWeakPassword", err)
	}
	if err := validatePassword("1234567"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("7 chars = %v, want ErrWeakPassword", err)
	}
	if err := validatePassword("12345678"); err != nil {
		t.Errorf("8 chars = %v, want ok", err)
	}
	if err := validatePassword("0p#n-9kQ*3"); err != nil {
		t.Errorf("strong password = %v, want ok", err)
	}
}

func TestSignupEnforcesPasswordPolicy(t *testing.T) {
	db := svcKeyDB(t)
	store := NewStore(db)
	svc := NewService(store)
	if _, err := svc.Signup(context.Background(), "pw-"+svcKeySuffix()+"@example.com", "abc", "x"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("short password signup = %v, want ErrWeakPassword", err)
	}
}

func TestChangePasswordEnforcesPolicy(t *testing.T) {
	db := svcKeyDB(t)
	u, store := newSvcKeyUser(t, db)
	svc := NewService(store)
	// First set a valid password via reset, then try a weak change.
	if err := svc.ResetPassword(context.Background(), u.ID, "old-pw-1234"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := svc.ChangePassword(context.Background(), u.ID, "old-pw-1234", "tiny"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak change = %v, want ErrWeakPassword", err)
	}
	if err := svc.ResetPassword(context.Background(), u.ID, "tiny"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak reset = %v, want ErrWeakPassword", err)
	}
	if err := svc.ChangePassword(context.Background(), u.ID, "old-pw-1234", "new-pw-5678"); err != nil {
		t.Fatalf("valid change = %v", err)
	}
}
