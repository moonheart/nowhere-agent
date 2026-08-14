package identity

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// tokenExpiry reads a token's stored expiry back from the DB.
func tokenExpiry(t *testing.T, db *sql.DB, tokenHash string) time.Time {
	t.Helper()
	var got time.Time
	if err := db.QueryRow(`SELECT expires_at FROM auth_tokens WHERE token_hash = $1`, tokenHash).Scan(&got); err != nil {
		t.Fatalf("read token expiry: %v", err)
	}
	return got
}

// Auth-token sliding-renewal tests run against the real dev Postgres like the
// rest of the identity PG tests; each user is unique and cleanup deletes only
// the rows the test created (auth_tokens cascade with their user).

// TestAuthTokenSlidingRenewal pins the sliding session: a successful
// Authenticate on a token whose remaining validity has dropped below the
// threshold extends it for a fresh full TTL, so an active account is never
// forced out by the absolute 30-day cap.
func TestAuthTokenSlidingRenewal(t *testing.T) {
	db := svcKeyDB(t)
	u, store := newSvcKeyUser(t, db)
	svc := NewService(store)

	raw, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := hashToken(raw)
	// Minted 25 days ago: 5 days left, below the 7-day renew threshold.
	exp := time.Now().Add(5 * 24 * time.Hour)
	if err := store.CreateToken(context.Background(), u.ID, hash, exp); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Authenticate(context.Background(), raw); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	got := tokenExpiry(t, db, hash)
	want := time.Now().Add(30 * 24 * time.Hour)
	if d := got.Sub(want); d < -time.Minute || d > time.Minute {
		t.Errorf("renewed expiry = %v, want ~%v (full TTL from now)", got, want)
	}
}

// TestAuthTokenNotRenewedAboveThreshold: a token with plenty of validity left
// must pass through untouched — renewal is low-frequency by construction.
func TestAuthTokenNotRenewedAboveThreshold(t *testing.T) {
	db := svcKeyDB(t)
	u, store := newSvcKeyUser(t, db)
	svc := NewService(store)

	raw, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := hashToken(raw)
	exp := time.Now().Add(20 * 24 * time.Hour)
	if err := store.CreateToken(context.Background(), u.ID, hash, exp); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Authenticate(context.Background(), raw); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	got := tokenExpiry(t, db, hash)
	if d := got.Sub(exp); d < -time.Minute || d > time.Minute {
		t.Errorf("expiry moved without renewal: was %v, now %v", exp, got)
	}
}

// TestAuthTokenRenewalDoesNotResurrectExpired: an already-expired token stays
// invalid — renewal is a convenience for valid sessions, not a second chance.
func TestAuthTokenRenewalDoesNotResurrectExpired(t *testing.T) {
	db := svcKeyDB(t)
	u, store := newSvcKeyUser(t, db)
	svc := NewService(store)

	raw, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := hashToken(raw)
	exp := time.Now().Add(-time.Hour)
	if err := store.CreateToken(context.Background(), u.ID, hash, exp); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Authenticate(context.Background(), raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("authenticate with expired token = %v, want ErrInvalidToken", err)
	}
	got := tokenExpiry(t, db, hash)
	if d := got.Sub(exp); d < -time.Minute || d > time.Minute {
		t.Errorf("expired token was extended: was %v, now %v", exp, got)
	}
}
