package identity

import (
	"context"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/secrets"
)

// The TOTP seed gets the same encryption-at-rest treatment as provider API
// keys: the raw row must be an AES-256-GCM envelope while the store returns
// plaintext to the service layer. Legacy plaintext rows (written before
// encryption was enabled) must keep reading, and a tampered envelope must
// fail closed.

const totpTestSeed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // RFC 6238 vector seed

func totpTestEncryptor(t *testing.T) *secrets.Encryptor {
	t.Helper()
	enc, err := secrets.NewSingle([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	return enc
}

func TestTOTPSecretEncryptedAtRest(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db).WithEncryption(totpTestEncryptor(t))
	u := mkUser(t, db)
	ctx := context.Background()

	if err := s.SetTOTP(ctx, u.ID, totpTestSeed, true); err != nil {
		t.Fatalf("set totp: %v", err)
	}

	// The raw column holds a ciphertext envelope, never the seed.
	var raw string
	if err := db.QueryRow(`SELECT totp_secret FROM users WHERE id = $1`, u.ID).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !secrets.IsEncrypted(raw) {
		t.Errorf("raw totp_secret = %q, want an enc:v1: envelope", raw)
	}
	if strings.Contains(raw, totpTestSeed[:8]) {
		t.Error("raw totp_secret contains the plaintext seed")
	}

	// The store still hands the plaintext seed to the service layer, so a
	// code generated from the seed verifies (the login second factor works).
	secret, enabled, err := s.TOTPState(ctx, u.ID)
	if err != nil {
		t.Fatalf("totp state: %v", err)
	}
	if secret != totpTestSeed || !enabled {
		t.Fatalf("totp state = (%q, %v), want the plaintext seed, enabled", secret, enabled)
	}
	code, err := totpCode(secret, time.Now())
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if ok, err := verifyTOTP(secret, code, time.Now()); err != nil || !ok {
		t.Fatalf("round-trip verify: ok=%v err=%v", ok, err)
	}

	// Clearing the factor stores NULL/empty, not an encrypted empty string.
	if err := s.SetTOTP(ctx, u.ID, "", false); err != nil {
		t.Fatalf("clear totp: %v", err)
	}
	secret, enabled, err = s.TOTPState(ctx, u.ID)
	if err != nil || secret != "" || enabled {
		t.Fatalf("cleared state = (%q, %v, %v)", secret, enabled, err)
	}
}

func TestTOTPSecretLegacyPlaintextReadsThrough(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db).WithEncryption(totpTestEncryptor(t))
	u := mkUser(t, db)
	ctx := context.Background()

	// Simulate a row written before encryption was enabled.
	if _, err := db.Exec(`UPDATE users SET totp_secret = $1, totp_enabled = true WHERE id = $2`, totpTestSeed, u.ID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	secret, enabled, err := s.TOTPState(ctx, u.ID)
	if err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	if secret != totpTestSeed || !enabled {
		t.Fatalf("legacy state = (%q, %v), want the plaintext seed, enabled", secret, enabled)
	}

	// The next write migrates the row to the envelope form.
	if err := s.SetTOTP(ctx, u.ID, totpTestSeed, true); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var raw string
	if err := db.QueryRow(`SELECT totp_secret FROM users WHERE id = $1`, u.ID).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if !secrets.IsEncrypted(raw) {
		t.Errorf("rewritten totp_secret = %q, want an enc:v1: envelope", raw)
	}
}

func TestTOTPSecretTamperedFailsClosed(t *testing.T) {
	db := pgTestDB(t)
	s := NewStore(db).WithEncryption(totpTestEncryptor(t))
	u := mkUser(t, db)

	// A value that LOOKS like an envelope but does not authenticate is
	// tampering/corruption: it must surface as an error, never be handed to
	// the TOTP verifier as a "plaintext seed".
	if _, err := db.Exec(`UPDATE users SET totp_secret = 'enc:v1:AAAAAAAAAAAAAAAAAAAAAA', totp_enabled = true WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("seed tampered row: %v", err)
	}
	if _, _, err := s.TOTPState(context.Background(), u.ID); err == nil {
		t.Fatal("tampered ciphertext read: want an error, got a silent pass-through")
	}
}
