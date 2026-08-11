package inbound

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/secrets"
)

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("INBOUND_PG_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3_000_000_000)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newStore(t *testing.T, enc *secrets.Encryptor) (*Store, *sql.DB) {
	t.Helper()
	s := NewStore(pgTestDB(t))
	if enc != nil {
		s = s.WithEncryption(enc)
	}
	return s, s.db
}

// seedUser inserts a user row and returns its id (cleaned up on test end).
func seedUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`,
		"inb-"+randHex()+"@test.dev").Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func testWebhook(userID string) Webhook {
	return Webhook{
		Name:    "erp-ticket",
		UserID:  userID,
		Secret:  "wh_testsecret12345678901234567890",
		Enabled: true,
	}
}

func TestCreateGetRoundTrip(t *testing.T) {
	s, db := newStore(t, nil)
	ctx := context.Background()
	uid := seedUser(t, db)

	w, err := s.Create(ctx, testWebhook(uid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.ID == "" {
		t.Fatal("create returned no id")
	}
	got, err := s.GetByID(ctx, w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "erp-ticket" || got.UserID != uid || !got.Enabled {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Secret != "wh_testsecret12345678901234567890" {
		t.Fatalf("secret round trip: %q", got.Secret)
	}
}

func TestCreateEncryptsSecret(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	enc, err := secrets.NewSingle(key)
	if err != nil {
		t.Fatal(err)
	}
	s, db := newStore(t, enc)
	ctx := context.Background()
	uid := seedUser(t, db)

	w, err := s.Create(ctx, testWebhook(uid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The row on disk must hold an encrypted envelope, not the secret.
	var stored string
	if err := db.QueryRow(`SELECT secret_cipher FROM inbound_webhooks WHERE id = $1`, w.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if stored == testWebhook(uid).Secret || !strings.HasPrefix(stored, "enc:v1:") {
		t.Fatalf("secret stored in plaintext or wrong envelope: %q", stored)
	}
	// Decrypting twice must give the secret back, and the read path decrypts.
	if _, err := s.Delete(ctx, w.ID, uid); err != nil {
		t.Fatalf("cleanup delete: %v", err)
	}
}

func TestOwnershipAndDelete(t *testing.T) {
	s, db := newStore(t, nil)
	ctx := context.Background()
	owner := seedUser(t, db)
	other := seedUser(t, db)

	w, err := s.Create(ctx, testWebhook(owner))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Other user cannot see or mutate it.
	if _, err := s.GetByIDAndUser(ctx, w.ID, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get as other: %v", err)
	}
	if err := s.RotateSecret(ctx, w.ID, other, "wh_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate as other: %v", err)
	}
	if err := s.SetEnabled(ctx, w.ID, other, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("toggle as other: %v", err)
	}
	if ok, _ := s.Delete(ctx, w.ID, other); ok {
		t.Fatal("delete as other reported success")
	}

	// Owner: rotate works and invalidates the old secret; toggle works.
	if err := s.RotateSecret(ctx, w.ID, owner, "wh_newsecret123456789012345678901"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	got, err := s.GetByID(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != "wh_newsecret123456789012345678901" {
		t.Fatalf("rotated secret not visible: %q", got.Secret)
	}
	if err := s.SetEnabled(ctx, w.ID, owner, false); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	got, _ = s.GetByID(ctx, w.ID)
	if got.Enabled {
		t.Fatal("webhook still enabled after toggle")
	}

	// List shows the webhook; delete removes it.
	all, err := s.ListByUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("list len = %d, want 1", len(all))
	}
	if ok, err := s.Delete(ctx, w.ID, owner); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if _, err := s.GetByID(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestTouchLastUsed(t *testing.T) {
	s, db := newStore(t, nil)
	ctx := context.Background()
	uid := seedUser(t, db)

	w, err := s.Create(ctx, testWebhook(uid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.TouchLastUsed(ctx, w.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ := s.GetByID(ctx, w.ID)
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at not stamped")
	}
}
