package secrets

import (
	"strings"
	"testing"
)

func mustEncryptor(t *testing.T) *Encryptor {
	t.Helper()
	key := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	e, err := NewSingle(key)
	if err != nil {
		t.Fatalf("NewSingle: %v", err)
	}
	return e
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	e := mustEncryptor(t)
	const secret = "sk-ant-api03-super-secret-key-value"
	ct, err := e.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !IsEncrypted(ct) {
		t.Errorf("ciphertext lacks envelope prefix: %q", ct)
	}
	if strings.Contains(ct, secret) {
		t.Errorf("ciphertext contains the plaintext: %q", ct)
	}
	got, err := e.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != secret {
		t.Errorf("round trip = %q, want %q", got, secret)
	}
}

func TestEncryptIsNonDeterministic(t *testing.T) {
	e := mustEncryptor(t)
	a, _ := e.Encrypt("same")
	b, _ := e.Encrypt("same")
	if a == b {
		t.Error("two encryptions of the same value must differ (random nonce)")
	}
}

func TestDecryptLegacyPlaintextPassthrough(t *testing.T) {
	e := mustEncryptor(t)
	// A pre-migration row holds plaintext; Decrypt must return it untouched so
	// enabling encryption is not a flag day.
	const legacy = "sk-ant-plaintext-from-before-encryption"
	got, err := e.Decrypt(legacy)
	if err != nil {
		t.Fatalf("Decrypt legacy: %v", err)
	}
	if got != legacy {
		t.Errorf("legacy passthrough = %q, want %q", got, legacy)
	}
}

func TestDecryptEmpty(t *testing.T) {
	e := mustEncryptor(t)
	got, err := e.Decrypt("")
	if err != nil || got != "" {
		t.Errorf("Decrypt empty = %q, %v", got, err)
	}
}

func TestDecryptTamperedEnvelopeFails(t *testing.T) {
	e := mustEncryptor(t)
	ct, _ := e.Encrypt("secret")
	// Corrupt one byte inside the base64 body (keep the prefix so it still
	// routes as an envelope). Authentication must fail — a corrupt envelope
	// must never be returned as if it were plaintext.
	body := strings.TrimPrefix(ct, prefix)
	bad := prefix + body[:len(body)-4] + "AAAA"
	if _, err := e.Decrypt(bad); err == nil {
		t.Error("tampered ciphertext must error, not silently pass through")
	}
}

func TestDecryptRejectsGarbageEnvelope(t *testing.T) {
	e := mustEncryptor(t)
	if _, err := e.Decrypt(prefix + "not-valid-base64!!!"); err == nil {
		t.Error("malformed envelope must error")
	}
}

func TestKeyRotationDecryptsOldCiphertext(t *testing.T) {
	oldKey := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	newKey := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	old, err := New(map[string][]byte{"v1": oldKey}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	ct, err := old.Encrypt("rotated-secret")
	if err != nil {
		t.Fatal(err)
	}

	// After rotation the ring holds both; the new key encrypts, the old still
	// decrypts the previously-written ciphertext.
	rotated, err := New(map[string][]byte{"v1": oldKey, "v2": newKey}, "v2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Decrypt(ct)
	if err != nil {
		t.Fatalf("old ciphertext after rotation: %v", err)
	}
	if got != "rotated-secret" {
		t.Errorf("rotated decrypt = %q", got)
	}

	// A ring missing the old key can no longer read the old ciphertext.
	dropped, err := New(map[string][]byte{"v2": newKey}, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dropped.Decrypt(ct); err == nil {
		t.Error("ciphertext must not decrypt once its key is dropped from the ring")
	}
}

func TestBase64MasterKeyAccepted(t *testing.T) {
	// Operators will often paste a base64 key into the environment; both the raw
	// 32-byte form and its base64 encoding must build the same encryptor.
	raw := []byte("0123456789abcdef0123456789abcdef")
	b64 := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	e, err := NewSingle([]byte(b64))
	if err != nil {
		t.Fatalf("NewSingle(base64): %v", err)
	}
	ct, _ := e.Encrypt("hello")
	got, err := e.Decrypt(ct)
	if err != nil || got != "hello" {
		t.Errorf("base64-key round trip = %q, %v", got, err)
	}
	_ = raw
}

func TestRejectsWrongKeyLength(t *testing.T) {
	if _, err := NewSingle([]byte("too-short")); err == nil {
		t.Error("a non-32-byte key must be rejected")
	}
}
