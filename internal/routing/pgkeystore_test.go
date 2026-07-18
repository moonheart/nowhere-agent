package routing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
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

	creds, err := ks.Resolve(context.Background(), userID)
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

	creds, err := ks.Resolve(context.Background(), userID)
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
	creds, err := ks.Resolve(context.Background(), other)
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

	if _, err := ks.Resolve(context.Background(), userID); err == nil {
		t.Error("expected error when no platform key and no team key")
	}
}
