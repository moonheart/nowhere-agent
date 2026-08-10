package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Service-key tests run against the real dev Postgres, like the rest of the
// identity PG tests; every row uses a unique owner and cleanup deletes only
// what the test created.

func svcKeyDSN() string {
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
}

func svcKeyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", svcKeyDSN())
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func svcKeySuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newSvcKeyUser(t *testing.T, db *sql.DB) (User, *Store) {
	t.Helper()
	store := NewStore(db)
	u, err := store.CreateUser(context.Background(),
		"svckey-"+svcKeySuffix()+"@example.com", "x", "svckey")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })
	return u, store
}

func TestServiceKeyIssueAuthenticateRevoke(t *testing.T) {
	db := svcKeyDB(t)
	u, store := newSvcKeyUser(t, db)
	svc := NewService(store)

	// Issue a non-expiring key; the raw token is sk_-prefixed and returned once.
	raw, key, err := svc.CreateServiceKey(context.Background(), "ci-bot", u.ID, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if raw[:3] != "sk_" {
		t.Fatalf("raw token prefix = %q, want sk_", raw[:3])
	}
	if key.Name != "ci-bot" || key.UserID != u.ID || key.ExpiresAt != nil {
		t.Fatalf("key mismatch: %+v", key)
	}

	// The key authenticates as its owner.
	authed, err := svc.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if authed.ID != u.ID {
		t.Fatalf("authenticated user = %s, want %s", authed.ID, u.ID)
	}

	// The raw token is not stored — only its hash.
	var storedHash string
	if err := db.QueryRow(`SELECT token_hash FROM service_keys WHERE id = $1`, key.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == raw || storedHash != hashToken(raw) {
		t.Fatal("service_keys must store the hash, not the raw token")
	}

	// Revoking kills the key.
	if err := svc.RevokeServiceKey(context.Background(), key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("authenticate after revoke = %v, want ErrInvalidToken", err)
	}
	// Revoking again is a not-found (already revoked).
	if err := svc.RevokeServiceKey(context.Background(), key.ID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("double revoke = %v, want ErrKeyNotFound", err)
	}
}

func TestServiceKeyTTLExpires(t *testing.T) {
	db := svcKeyDB(t)
	u, store := newSvcKeyUser(t, db)
	svc := NewService(store)

	raw, key, err := svc.CreateServiceKey(context.Background(), "short-lived", u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if key.ExpiresAt == nil {
		t.Fatal("ttl key should carry expires_at")
	}
	if _, err := svc.Authenticate(context.Background(), raw); err != nil {
		t.Fatalf("authenticate before expiry: %v", err)
	}

	// Simulate time passing: the store decides expiry against `now`, so a key
	// whose expiration has passed must read invalid.
	if _, err := store.UserIDByServiceKeyHash(context.Background(), hashToken(raw), key.ExpiresAt.Add(time.Minute)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("authenticate after expiry = %v, want ErrInvalidToken", err)
	}
}

func TestServiceKeyScopedToDisabledOwner(t *testing.T) {
	db := svcKeyDB(t)
	u, store := newSvcKeyUser(t, db)
	svc := NewService(store)

	raw, _, err := svc.CreateServiceKey(context.Background(), "owner-disabled", u.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Disabling the owner disables the key (Authenticate re-checks the account).
	if err := store.SetUserDisabled(context.Background(), u.ID, true); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), raw); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("authenticate with disabled owner = %v, want ErrUserDisabled", err)
	}
}

func TestServiceKeyCreateUnknownUser(t *testing.T) {
	db := svcKeyDB(t)
	store := NewStore(db)
	svc := NewService(store)
	if _, _, err := svc.CreateServiceKey(context.Background(), "ghost", "00000000-0000-0000-0000-000000000000", 0); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("create for unknown user = %v, want ErrUserNotFound", err)
	}
}
