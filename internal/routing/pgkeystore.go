package routing

import (
	"context"
	"database/sql"
	"fmt"
)

// PGKeyStore resolves credentials from Postgres: a team-configured key when
// one of the user's teams has set an override, otherwise the platform key
// supplied at construction.
type PGKeyStore struct {
	db          *sql.DB
	platformKey string
}

// NewPGKeyStore creates a Postgres-backed KeyStore. platformKey is used when
// no team override applies.
func NewPGKeyStore(db *sql.DB, platformKey string) *PGKeyStore {
	return &PGKeyStore{db: db, platformKey: platformKey}
}

// Resolve returns credentials for the user: the team key if any team the user
// belongs to has configured one (deterministic — lowest team id wins when
// several teams set keys), otherwise the platform key.
func (s *PGKeyStore) Resolve(ctx context.Context, userID string) (Credentials, error) {
	var teamID, apiKey string
	err := s.db.QueryRowContext(ctx, `
		SELECT k.team_id, k.api_key
		FROM team_api_keys k
		JOIN team_memberships m ON m.team_id = k.team_id
		WHERE m.user_id = $1
		ORDER BY k.team_id
		LIMIT 1`, userID).Scan(&teamID, &apiKey)
	if err == nil {
		return Credentials{APIKey: apiKey, TeamID: teamID}, nil
	}
	if err != sql.ErrNoRows {
		return Credentials{}, fmt.Errorf("resolve team key: %w", err)
	}
	if s.platformKey == "" {
		return Credentials{}, fmt.Errorf("no platform key configured and no team key for user %s", userID)
	}
	return Credentials{APIKey: s.platformKey, Platform: true}, nil
}
