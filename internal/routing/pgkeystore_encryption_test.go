package routing

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/secrets"
)

func testEncryptor(t *testing.T) *secrets.Encryptor {
	t.Helper()
	e, err := secrets.NewSingle([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewSingle: %v", err)
	}
	return e
}

// With encryption on, a written key must be stored as a self-describing
// envelope (never the plaintext) yet Resolve must hand back the original key.
func TestPGKeyStoreEncryptedRoundTrip(t *testing.T) {
	db := pgTestDB(t)
	userID, teamID := pgUserTeam(t, db)
	ks := NewPGKeyStore(db, "platform-key").WithEncryption(testEncryptor(t))
	ctx := context.Background()

	const secret = "sk-ant-encrypted-secret-4242"
	if _, err := ks.UpsertTeamKey(ctx, teamID, "anthropic", secret); err != nil {
		t.Fatalf("UpsertTeamKey: %v", err)
	}

	// The row itself must hold an envelope, not the secret.
	var stored string
	if err := db.QueryRow(`SELECT api_key FROM team_api_keys WHERE team_id=$1 AND provider='anthropic'`, teamID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !secrets.IsEncrypted(stored) {
		t.Errorf("stored value is not an envelope: %q", stored)
	}
	if strings.Contains(stored, secret) {
		t.Errorf("stored value leaks the plaintext: %q", stored)
	}

	// Resolve decrypts transparently.
	creds, err := ks.Resolve(ctx, userID, "anthropic")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if creds.APIKey != secret || creds.TeamID != teamID {
		t.Errorf("Resolve = %+v, want the decrypted secret", creds)
	}
}

// A key written BEFORE encryption was enabled (plaintext row) must still
// resolve after encryption is turned on — enabling it is a migration, not a
// flag day. And the console mask must reflect the real key, not the envelope.
func TestPGKeyStoreDecryptsLegacyPlaintextRow(t *testing.T) {
	db := pgTestDB(t)
	userID, teamID := pgUserTeam(t, db)
	ctx := context.Background()

	// Write plaintext directly (simulating a pre-encryption row).
	const legacy = "sk-ant-legacy-plain-9876"
	if _, err := db.Exec(`INSERT INTO team_api_keys (team_id, provider, api_key) VALUES ($1,'anthropic',$2)`, teamID, legacy); err != nil {
		t.Fatal(err)
	}

	ks := NewPGKeyStore(db, "platform-key").WithEncryption(testEncryptor(t))
	creds, err := ks.Resolve(ctx, userID, "anthropic")
	if err != nil {
		t.Fatalf("Resolve legacy: %v", err)
	}
	if creds.APIKey != legacy {
		t.Errorf("legacy row resolved to %q, want %q", creds.APIKey, legacy)
	}

	keys, err := ks.ListTeamKeys(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Masked != "••••9876" {
		t.Errorf("legacy row mask = %+v, want the real last-four ••••9876", keys)
	}

	// Rewriting the row (rotation) re-encrypts it.
	if _, err := ks.UpsertTeamKey(ctx, teamID, "anthropic", "sk-ant-new-5555"); err != nil {
		t.Fatal(err)
	}
	var stored string
	db.QueryRow(`SELECT api_key FROM team_api_keys WHERE team_id=$1 AND provider='anthropic'`, teamID).Scan(&stored)
	if !secrets.IsEncrypted(stored) {
		t.Errorf("rotated row should now be encrypted, got %q", stored)
	}
}

// The mask must come from the plaintext key, so the console shows the same
// last-four the operator recognises, not the tail of a base64 envelope.
func TestPGKeyStoreMaskComesFromPlaintext(t *testing.T) {
	db := pgTestDB(t)
	_, teamID := pgUserTeam(t, db)
	ks := NewPGKeyStore(db, "platform-key").WithEncryption(testEncryptor(t))
	ctx := context.Background()

	if _, err := ks.UpsertTeamKey(ctx, teamID, "anthropic", "sk-ant-visible-tail-4321"); err != nil {
		t.Fatal(err)
	}
	keys, err := ks.ListTeamKeys(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Masked != "••••4321" {
		t.Errorf("mask = %+v, want ••••4321 from the plaintext, not envelope tail", keys)
	}
	if strings.Contains(keys[0].Masked, "enc:v1") {
		t.Errorf("mask leaks the envelope: %q", keys[0].Masked)
	}
}

// A row corrupted under a DIFFERENT master key must fail Resolve loudly (so the
// caller falls back to the platform key and the anomaly is logged), never
// silently ship a garbage credential to the provider.
func TestPGKeyStoreUndecryptableRowFailsResolve(t *testing.T) {
	db := pgTestDB(t)
	userID, teamID := pgUserTeam(t, db)
	ctx := context.Background()

	other, err := secrets.NewSingle([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}
	ksOther := NewPGKeyStore(db, "platform-key").WithEncryption(other)
	if _, err := ksOther.UpsertTeamKey(ctx, teamID, "anthropic", "sk-ant-secret"); err != nil {
		t.Fatal(err)
	}

	ksWrong := NewPGKeyStore(db, "platform-key").WithEncryption(testEncryptor(t))
	if _, err := ksWrong.Resolve(ctx, userID, "anthropic"); err == nil {
		t.Error("a row encrypted under a different key must fail Resolve, not pass through")
	}
}

// With no Encryptor configured the store keeps its legacy plaintext behaviour —
// a deployment without a master key still boots and resolves keys.
func TestPGKeyStoreNoEncryptorStaysPlaintext(t *testing.T) {
	db := pgTestDB(t)
	userID, teamID := pgUserTeam(t, db)
	ks := NewPGKeyStore(db, "platform-key") // no WithEncryption
	ctx := context.Background()

	if _, err := ks.UpsertTeamKey(ctx, teamID, "anthropic", "sk-ant-plain-7777"); err != nil {
		t.Fatal(err)
	}
	var stored string
	db.QueryRow(`SELECT api_key FROM team_api_keys WHERE team_id=$1 AND provider='anthropic'`, teamID).Scan(&stored)
	if secrets.IsEncrypted(stored) {
		t.Errorf("without an encryptor the row should be plaintext, got %q", stored)
	}
	creds, err := ks.Resolve(ctx, userID, "anthropic")
	if err != nil || creds.APIKey != "sk-ant-plain-7777" {
		t.Errorf("plaintext resolve = %+v, %v", creds, err)
	}
}
