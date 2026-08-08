package routing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"nowhere-agent/internal/secrets"
)

// PGKeyStore resolves credentials from Postgres: a team-configured key when
// one of the user's teams has set an override for the provider being called,
// otherwise the platform key supplied at construction. It also owns the
// management path for those keys (admin-console) — the table is its own.
//
// Keys are encrypted at rest (enterprise-readiness P0-2): when an Encryptor is
// configured, writes encrypt and reads decrypt, transparently to callers.
// Rows written before encryption was enabled are plaintext; they still read
// (Decrypt passes them through) and are re-encrypted on the next write, so
// enabling encryption is a gradual migration, not a flag day.
type PGKeyStore struct {
	db          *sql.DB
	platformKey string
	// enc encrypts team keys at rest; nil stores plaintext (legacy/dev). The
	// resolution path (Resolve) decrypts; the console paths never see plaintext
	// regardless — ListTeamKeys masks.
	enc *secrets.Encryptor
}

// NewPGKeyStore creates a Postgres-backed KeyStore. platformKey is used when
// no team override applies.
func NewPGKeyStore(db *sql.DB, platformKey string) *PGKeyStore {
	return &PGKeyStore{db: db, platformKey: platformKey}
}

// WithEncryption enables encryption-at-rest for team keys. Passing it is how
// the server turns SECRETS_MASTER_KEY into protection; leaving it nil keeps
// the legacy plaintext behaviour so a deployment without a configured master
// key still boots (and logs that keys are unprotected).
func (s *PGKeyStore) WithEncryption(enc *secrets.Encryptor) *PGKeyStore {
	s.enc = enc
	return s
}

// Resolve returns credentials for the user's call to the named provider: the
// team key if any team the user belongs to has configured one FOR THAT PROVIDER
// (deterministic — lowest team id wins when several teams have), otherwise the
// platform key.
//
// The provider filter matters: a team that configured an OpenAI key must not
// have it handed to an Anthropic call, which would ship the secret to the wrong
// vendor and fail to authenticate there.
func (s *PGKeyStore) Resolve(ctx context.Context, userID, provider string) (Credentials, error) {
	var teamID, apiKey string
	err := s.db.QueryRowContext(ctx, `
		SELECT k.team_id, k.api_key
		FROM team_api_keys k
		JOIN team_memberships m ON m.team_id = k.team_id
		WHERE m.user_id = $1 AND k.provider = $2
		ORDER BY k.team_id
		LIMIT 1`, userID, provider).Scan(&teamID, &apiKey)
	if err == nil {
		plain, derr := s.decrypt(apiKey)
		if derr != nil {
			// A row that will not decrypt is a security-relevant failure (wrong
			// key, tampering), not a "no key" — surface it so the caller falls
			// back to the platform key and the server logs the anomaly, rather
			// than silently shipping a corrupt credential to the provider.
			return Credentials{}, fmt.Errorf("decrypt team key: %w", derr)
		}
		return Credentials{APIKey: plain, TeamID: teamID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Credentials{}, fmt.Errorf("resolve team key: %w", err)
	}
	if s.platformKey == "" {
		return Credentials{}, fmt.Errorf("no platform key configured and no %s key for user %s", provider, userID)
	}
	return Credentials{APIKey: s.platformKey, Platform: true}, nil
}

// encrypt protects a key for storage; decrypt reverses it. Both are no-ops when
// no Encryptor is configured (legacy plaintext), so call sites branch nothing.
func (s *PGKeyStore) encrypt(plain string) (string, error) {
	if s.enc == nil {
		return plain, nil
	}
	return s.enc.Encrypt(plain)
}

func (s *PGKeyStore) decrypt(stored string) (string, error) {
	if s.enc == nil {
		return stored, nil
	}
	return s.enc.Decrypt(stored)
}

// TeamKey is a team's configured provider credential as the console displays
// it. The secret itself is never carried: Masked holds only enough to tell two
// keys apart, so a compromised team-admin session cannot read keys back out.
type TeamKey struct {
	Provider  string
	Masked    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListTeamKeys returns the team's configured providers with masked keys.
func (s *PGKeyStore) ListTeamKeys(ctx context.Context, teamID string) ([]TeamKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, api_key, created_at, updated_at
		FROM team_api_keys
		WHERE team_id = $1
		ORDER BY provider`, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team keys: %w", err)
	}
	defer rows.Close()

	var out []TeamKey
	for rows.Next() {
		var (
			k   TeamKey
			raw string
		)
		if err := rows.Scan(&k.Provider, &raw, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan team key: %w", err)
		}
		// Mask the DECRYPTED key, not the stored envelope — masking the ciphertext
		// would show the tail of the base64 envelope, which is neither the key's
		// real last-four nor a stable identity for it. A row that fails to
		// decrypt masks to the opaque placeholder rather than erroring the whole
		// listing (the resolution path already surfaces decrypt failures loudly).
		plain, err := s.decrypt(raw)
		if err != nil {
			k.Masked = MaskKey("")
		} else {
			k.Masked = MaskKey(plain)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpsertTeamKey sets or rotates the team's key for a provider. The key is
// encrypted before it is written; the returned TeamKey is masked from the
// caller-supplied plaintext (which the caller already has).
func (s *PGKeyStore) UpsertTeamKey(ctx context.Context, teamID, provider, apiKey string) (TeamKey, error) {
	stored, err := s.encrypt(apiKey)
	if err != nil {
		return TeamKey{}, fmt.Errorf("encrypt team key: %w", err)
	}
	var k TeamKey
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO team_api_keys (team_id, provider, api_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (team_id, provider)
		DO UPDATE SET api_key = EXCLUDED.api_key, updated_at = now()
		RETURNING provider, created_at, updated_at`,
		teamID, provider, stored).Scan(&k.Provider, &k.CreatedAt, &k.UpdatedAt)
	if err != nil {
		return TeamKey{}, fmt.Errorf("upsert team key: %w", err)
	}
	k.Masked = MaskKey(apiKey)
	return k, nil
}

// DeleteTeamKey removes the team's key for a provider. It reports whether a key
// was there to delete.
func (s *PGKeyStore) DeleteTeamKey(ctx context.Context, teamID, provider string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM team_api_keys WHERE team_id = $1 AND provider = $2`, teamID, provider)
	if err != nil {
		return false, fmt.Errorf("delete team key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MaskKey renders a credential as a display fragment: the last four characters
// behind a fixed-width ellipsis. The width is fixed rather than proportional to
// the key, so the rendering leaks nothing about the secret's length. A key too
// short to have a distinguishable tail masks entirely.
func MaskKey(raw string) string {
	const keep = 4
	if len(raw) <= keep {
		return "••••"
	}
	return "••••" + raw[len(raw)-keep:]
}
