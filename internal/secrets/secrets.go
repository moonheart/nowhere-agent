// Package secrets provides encryption-at-rest for the credentials the platform
// stores (enterprise-readiness P0-2). The one real secret nowhere-agent
// persists is each team's LLM provider API key; before this package those keys
// sat in Postgres as plaintext, so a database dump or a read on the wrong
// connection handed them out.
//
// The scheme is deliberately simple and self-contained (no external KMS
// dependency — appropriate for a self-hosted internal platform):
//
//   - AES-256-GCM authenticated encryption. A random 96-bit nonce per value is
//     prepended to the ciphertext, so two encryptions of the same key differ.
//   - The data key comes from the environment (SECRETS_MASTER_KEY), as raw
//     bytes or base64. Key rotation is supported by carrying a key id in the
//     ciphertext header and keeping an ordered key ring; see RotateKeys.
//   - Ciphertexts are self-describing: "enc:v1:<base64(nonce||ct)>". The prefix
//     lets the reader distinguish encrypted rows from legacy plaintext ones, so
//     enabling encryption is a gradual migration, not a flag day — plaintext
//     rows still read fine, and are re-encrypted the next time they are written.
//
// Threat model: this defends against read-only database compromise (a leaked
// backup, an over-permissive role, a SQL-injection read). It does NOT defend
// against an attacker who reads the server's environment — the master key has
// to live somewhere, and for a self-hosted single-binary deploy the environment
// is the accepted root of trust. Sites that need stronger isolation can swap
// the KeySource for a KMS-backed one without touching the envelope format.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// prefix marks a value as encrypted and names the envelope version, so the
// reader can route it and a future v2 (say, a KMS-wrapped key) can coexist.
const prefix = "enc:v1:"

// ErrNoKey is returned by Encrypt when no data key is configured.
var ErrNoKey = errors.New("secrets: no master key configured")

// ErrCiphertext is returned by Decrypt for a malformed or corrupt envelope.
var ErrCiphertext = errors.New("secrets: malformed ciphertext")

// Encryptor encrypts and decrypts secret strings at rest. It is safe for
// concurrent use. The zero value is unusable; build one with New.
type Encryptor struct {
	// ring maps key id -> AEAD. The active (encryption) key is activeID. Only
	// the active key encrypts; every key in the ring decrypts, which is what
	// makes rotation safe: old ciphertexts keep decrypting after a new key
	// becomes active.
	ring     map[string]cipher.AEAD
	activeID string
}

// New builds an Encryptor from an ordered key ring: a map of key id to 32-byte
// data key, plus the id of the key that encrypts. At least one key is required.
// In practice the ring has one entry (the current master key) until a rotation.
func New(keys map[string][]byte, activeID string) (*Encryptor, error) {
	if len(keys) == 0 {
		return nil, ErrNoKey
	}
	if _, ok := keys[activeID]; !ok {
		return nil, fmt.Errorf("secrets: active key id %q not in ring", activeID)
	}
	ring := make(map[string]cipher.AEAD, len(keys))
	for id, raw := range keys {
		if len(raw) != 32 {
			return nil, fmt.Errorf("secrets: key %q is %d bytes, want 32 (AES-256)", id, len(raw))
		}
		block, err := aes.NewCipher(raw)
		if err != nil {
			return nil, fmt.Errorf("secrets: key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("secrets: key %q: %w", id, err)
		}
		ring[id] = aead
	}
	return &Encryptor{ring: ring, activeID: activeID}, nil
}

// NewSingle builds an Encryptor from one data key (the common case: no rotation
// in flight). key may be raw bytes or base64 (standard or raw encoding); it
// must decode to 32 bytes.
func NewSingle(key []byte) (*Encryptor, error) {
	raw, err := normalizeKey(key)
	if err != nil {
		return nil, err
	}
	return New(map[string][]byte{"v1": raw}, "v1")
}

// normalizeKey accepts a 32-byte key as-is or base64-encoded and returns the
// raw 32 bytes, so operators can put either form in the environment.
func normalizeKey(key []byte) ([]byte, error) {
	if len(key) == 32 {
		return key, nil
	}
	s := strings.TrimSpace(string(key))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err := enc.DecodeString(s); err == nil && len(raw) == 32 {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("secrets: master key must be 32 bytes or base64 of 32 bytes (got %d bytes)", len(key))
}

// Encrypt returns plaintext as a self-describing envelope. A "" plaintext is
// returned as "" (there is no secret to protect, and callers round-trip empty).
func (e *Encryptor) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	aead := e.ring[e.activeID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: nonce: %w", err)
	}
	ct := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt. It recognises three forms:
//
//   - an envelope ("enc:v1:…")   → decrypt with the ring
//   - ""                          → "" (no secret)
//   - anything else               → treated as legacy plaintext and returned
//     as-is, so pre-encryption rows keep working until rewritten
//
// A value that LOOKS like an envelope but fails to decode or authenticate is an
// error, not a silent pass-through — that is tampering or corruption, and
// returning it as "plaintext" would hand the caller garbage.
func (e *Encryptor) Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !IsEncrypted(value) {
		return value, nil // legacy plaintext, pre-migration row
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("%w: bad base64", ErrCiphertext)
	}
	// Try every key in the ring. GCM authentication fails for the wrong key, so
	// this is safe; with a single-key ring it is one attempt.
	for _, aead := range e.ring {
		if len(raw) < aead.NonceSize() {
			break
		}
		nonce, ct := raw[:aead.NonceSize()], raw[aead.NonceSize():]
		if pt, err := aead.Open(nil, nonce, ct, nil); err == nil {
			return string(pt), nil
		}
	}
	return "", fmt.Errorf("%w: cannot authenticate with any configured key", ErrCiphertext)
}

// IsEncrypted reports whether value carries the envelope prefix. Exported so a
// store can decide whether a rewrite is due (a plaintext row is due).
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, prefix)
}
