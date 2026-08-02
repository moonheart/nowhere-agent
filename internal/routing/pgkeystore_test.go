package routing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := getenvOr("ROUTING_PG_TEST_DSN", "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable")
	db, err := sql.Open("pgx", dsn)
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

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pgUserTeam creates a user and a team with the user as a member; cleans up.
func pgUserTeam(t *testing.T, db *sql.DB) (userID, teamID string) {
	t.Helper()
	err := db.QueryRow(`
		INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		"rk-"+randHex()+"@example.com").Scan(&userID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	err = db.QueryRow(`INSERT INTO teams (name) VALUES ($1) RETURNING id`, "t-"+randHex()).Scan(&teamID)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1,$2,'owner')`, teamID, userID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM teams WHERE id = $1`, teamID)
		db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, teamID
}

func TestPGKeyStorePlatformFallback(t *testing.T) {
	db := pgTestDB(t)
	userID, _ := pgUserTeam(t, db)
	ks := NewPGKeyStore(db, "platform-key")

	creds, err := ks.Resolve(context.Background(), userID, "anthropic")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !creds.Platform || creds.APIKey != "platform-key" || creds.TeamID != "" {
		t.Errorf("expected platform key, got %+v", creds)
	}
}

func TestPGKeyStoreTeamOverride(t *testing.T) {
	db := pgTestDB(t)
	userID, teamID := pgUserTeam(t, db)
	if _, err := db.Exec(`INSERT INTO team_api_keys (team_id, provider, api_key) VALUES ($1,'anthropic','team-key')`, teamID); err != nil {
		t.Fatal(err)
	}
	ks := NewPGKeyStore(db, "platform-key")

	creds, err := ks.Resolve(context.Background(), userID, "anthropic")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if creds.Platform || creds.APIKey != "team-key" || creds.TeamID != teamID {
		t.Errorf("expected team key override, got %+v", creds)
	}
}

func TestPGKeyStoreNoTeamKeyForNonMember(t *testing.T) {
	db := pgTestDB(t)
	_, teamID := pgUserTeam(t, db) // team has a key...
	if _, err := db.Exec(`INSERT INTO team_api_keys (team_id, provider, api_key) VALUES ($1,'anthropic','team-key')`, teamID); err != nil {
		t.Fatal(err)
	}
	// ...but this user is not a member.
	var other string
	if err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`, "rk-"+randHex()+"@example.com").Scan(&other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, other) })

	ks := NewPGKeyStore(db, "platform-key")
	creds, err := ks.Resolve(context.Background(), other, "anthropic")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !creds.Platform {
		t.Errorf("non-member should get platform key, got %+v", creds)
	}
}

func TestPGKeyStoreNoKeyAtAll(t *testing.T) {
	db := pgTestDB(t)
	userID, _ := pgUserTeam(t, db)
	ks := NewPGKeyStore(db, "") // no platform key

	if _, err := ks.Resolve(context.Background(), userID, "anthropic"); err == nil {
		t.Error("expected error when no platform key and no team key")
	}
}

// A team key configured for one provider must not be handed to a call to a
// different provider: that would ship the secret to the wrong vendor.
func TestPGKeyStoreIgnoresOtherProvidersKey(t *testing.T) {
	db := pgTestDB(t)
	userID, teamID := pgUserTeam(t, db)
	if _, err := db.Exec(`INSERT INTO team_api_keys (team_id, provider, api_key) VALUES ($1,'openai','openai-team-key')`, teamID); err != nil {
		t.Fatal(err)
	}
	ks := NewPGKeyStore(db, "platform-key")

	creds, err := ks.Resolve(context.Background(), userID, "anthropic")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !creds.Platform || creds.APIKey != "platform-key" {
		t.Errorf("anthropic call must not use the openai team key, got %+v", creds)
	}

	creds, err = ks.Resolve(context.Background(), userID, "openai")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if creds.APIKey != "openai-team-key" || creds.TeamID != teamID {
		t.Errorf("openai call should use the team key, got %+v", creds)
	}
}

func TestPGKeyStoreUpsertListDelete(t *testing.T) {
	db := pgTestDB(t)
	_, teamID := pgUserTeam(t, db)
	ks := NewPGKeyStore(db, "platform-key")
	ctx := context.Background()

	if _, err := ks.UpsertTeamKey(ctx, teamID, "anthropic", "sk-ant-abcd1234"); err != nil {
		t.Fatalf("UpsertTeamKey: %v", err)
	}
	keys, err := ks.ListTeamKeys(ctx, teamID)
	if err != nil {
		t.Fatalf("ListTeamKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Provider != "anthropic" {
		t.Fatalf("keys = %+v, want one anthropic entry", keys)
	}
	if keys[0].Masked != "••••1234" {
		t.Errorf("masked = %q, want the last four behind an ellipsis", keys[0].Masked)
	}

	// Rotation replaces the stored secret rather than adding a second row.
	if _, err := ks.UpsertTeamKey(ctx, teamID, "anthropic", "sk-ant-wxyz9876"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	keys, err = ks.ListTeamKeys(ctx, teamID)
	if err != nil {
		t.Fatalf("ListTeamKeys after rotate: %v", err)
	}
	if len(keys) != 1 || keys[0].Masked != "••••9876" {
		t.Fatalf("after rotation keys = %+v, want one entry masked ••••9876", keys)
	}

	deleted, err := ks.DeleteTeamKey(ctx, teamID, "anthropic")
	if err != nil || !deleted {
		t.Fatalf("DeleteTeamKey = %v, %v; want true, nil", deleted, err)
	}
	deleted, err = ks.DeleteTeamKey(ctx, teamID, "anthropic")
	if err != nil || deleted {
		t.Errorf("second delete = %v, %v; want false, nil", deleted, err)
	}
}

// The listing is the only read path for keys, and it must never carry the
// secret — the console has no reason to display one back.
func TestListTeamKeysNeverReturnsPlaintext(t *testing.T) {
	db := pgTestDB(t)
	_, teamID := pgUserTeam(t, db)
	ks := NewPGKeyStore(db, "platform-key")
	ctx := context.Background()

	const secret = "sk-ant-super-secret-value"
	if _, err := ks.UpsertTeamKey(ctx, teamID, "anthropic", secret); err != nil {
		t.Fatal(err)
	}
	keys, err := ks.ListTeamKeys(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if strings.Contains(k.Masked, "super-secret") || k.Masked == secret {
			t.Errorf("masked value leaks the secret: %q", k.Masked)
		}
	}
}

func TestMaskKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sk-ant-abcd1234", "••••1234"},
		{"abcd", "••••"},
		{"abc", "••••"},
		{"", "••••"},
	}
	for _, c := range cases {
		if got := MaskKey(c.in); got != c.want {
			t.Errorf("MaskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Masking must not reveal how long the secret is: two keys of very different
// lengths render at the same width.
func TestMaskKeyHidesLength(t *testing.T) {
	short := MaskKey("sk-a1234")
	long := MaskKey("sk-a-very-much-longer-key-1234")
	if len(short) != len(long) {
		t.Errorf("mask width leaks key length: %q vs %q", short, long)
	}
}
