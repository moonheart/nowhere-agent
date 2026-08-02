package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store persists identity data in Postgres.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// userColumns is the projection every user query shares, in the order scanUser
// expects. Kept in one place so adding a column touches one line.
const userColumns = `id, email, display_name, password_hash, platform_role, disabled_at, created_at, updated_at`

// bootstrapAdminLockKey serializes the "is this the first account?" check in
// CreateUser. There is no row to lock on an empty table, so two concurrent
// signups would both see zero users and both become admins; a transaction-scoped
// advisory lock is the only thing that orders them. Released at commit.
const bootstrapAdminLockKey = `nowhere.bootstrap_admin`

// CreateUser inserts a user. Returns ErrUserExists on duplicate email.
//
// The first account on an empty platform is created as a platform admin, so a
// fresh deployment always has someone who can administer it. Deployments whose
// accounts predate the role designate one via BOOTSTRAP_ADMIN_EMAIL instead
// (see Service.PromoteByEmail).
func (s *Store) CreateUser(ctx context.Context, email, passwordHash, displayName string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin create user: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, bootstrapAdminLockKey); err != nil {
		return User{}, fmt.Errorf("bootstrap lock: %w", err)
	}

	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
		return User{}, fmt.Errorf("count users: %w", err)
	}
	role := PlatformRoleUser
	if existing == 0 {
		role = PlatformRoleAdmin
	}

	u, err := scanUserRow(tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, display_name, platform_role)
		VALUES ($1, $2, $3, $4)
		RETURNING `+userColumns,
		email, passwordHash, displayName, string(role),
	))
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrUserExists
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit create user: %w", err)
	}
	return u, nil
}

// UserByEmail fetches a user by email (for login). The match is exact: the
// users.email unique constraint is case-sensitive, so a case-insensitive lookup
// could match two distinct accounts and silently pick one.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.scanUser(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email)
}

// UserByID fetches a user by id.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return s.scanUser(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
}

func (s *Store) scanUser(ctx context.Context, q string, arg any) (User, error) {
	u, err := scanUserRow(s.db.QueryRowContext(ctx, q, arg))
	// A malformed id is a lookup for something that cannot exist, not a server
	// fault — ids arrive from URL path segments.
	if errors.Is(err, sql.ErrNoRows) || IsMalformedID(err) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("query user: %w", err)
	}
	return u, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so the user
// projection is decoded in one place regardless of query shape.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanUserRow decodes the userColumns projection. It returns sql.ErrNoRows
// unwrapped so callers can map it to their own not-found error.
func scanUserRow(row rowScanner) (User, error) {
	var (
		u        User
		role     string
		disabled sql.NullTime
	)
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &role, &disabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, err
	}
	u.PlatformRole = PlatformRole(role)
	if disabled.Valid {
		t := disabled.Time
		u.DisabledAt = &t
	}
	return u, nil
}

// CreateToken stores a hashed token for a user.
func (s *Store) CreateToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, tokenHash, expiresAt)
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}

// UserIDByTokenHash resolves a valid (unexpired) token to its user id.
func (s *Store) UserIDByTokenHash(ctx context.Context, tokenHash string, now time.Time) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id FROM auth_tokens WHERE token_hash = $1 AND expires_at > $2`,
		tokenHash, now).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("resolve token: %w", err)
	}
	return userID, nil
}

// DeleteToken removes a token (logout).
func (s *Store) DeleteToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_tokens WHERE token_hash = $1`, tokenHash)
	return err
}

// CreateTeam inserts a team and makes the creator its owner.
func (s *Store) CreateTeam(ctx context.Context, name, ownerUserID string) (Team, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Team{}, err
	}
	defer tx.Rollback()

	var t Team
	err = tx.QueryRowContext(ctx, `
		INSERT INTO teams (name) VALUES ($1) RETURNING id, name, created_at, updated_at`, name).
		Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Team{}, fmt.Errorf("create team: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1, $2, $3)`,
		t.ID, ownerUserID, string(RoleOwner)); err != nil {
		return Team{}, fmt.Errorf("add owner membership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Team{}, err
	}
	return t, nil
}

// AddMember adds a user to a team with a role.
func (s *Store) AddMember(ctx context.Context, teamID, userID string, role Role) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		teamID, userID, string(role))
	return err
}

// TeamIDsForUser returns the ids of teams the user belongs to.
func (s *Store) TeamIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT team_id FROM team_memberships WHERE user_id = $1 ORDER BY team_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// IsMember reports whether the user belongs to the team.
func (s *Store) IsMember(ctx context.Context, teamID, userID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM team_memberships WHERE team_id = $1 AND user_id = $2`,
		teamID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
