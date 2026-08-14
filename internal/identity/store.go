package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store persists identity data in Postgres.
type Store struct {
	db *sql.DB
	// firstAccountAdmin, when true, makes the first account created on an
	// empty platform a platform admin (the legacy bootstrap). Off by default:
	// on a public deployment the first random signup must not claim the admin
	// role before operations can — only a deployment that explicitly set
	// BOOTSTRAP_ADMIN_EMAIL opts in (see cmd/server wiring).
	firstAccountAdmin bool
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// WithFirstAccountAdmin toggles the "first account on an empty platform
// becomes admin" bootstrap. Off by default; cmd/server enables it exactly when
// BOOTSTRAP_ADMIN_EMAIL is set.
func (s *Store) WithFirstAccountAdmin(enabled bool) *Store {
	s.firstAccountAdmin = enabled
	return s
}

// userColumns is the projection every user query shares, in the order scanUser
// expects. Kept in one place so adding a column touches one line.
const userColumns = `id, email, display_name, password_hash, platform_role, disabled_at, created_at, updated_at, phone`

// bootstrapAdminLockKey serializes the "is this the first account?" check in
// CreateUser. There is no row to lock on an empty table, so two concurrent
// signups would both see zero users and both become admins; a transaction-scoped
// advisory lock is the only thing that orders them. Released at commit.
const bootstrapAdminLockKey = `nowhere.bootstrap_admin`

// CreateUser inserts a user. Returns ErrUserExists on duplicate email.
//
// With the first-account bootstrap enabled (see WithFirstAccountAdmin, wired
// from BOOTSTRAP_ADMIN_EMAIL being set), the first account on an empty
// platform is created as a platform admin, so a fresh deployment always has
// someone who can administer it. Deployments whose accounts predate the role
// designate one via BOOTSTRAP_ADMIN_EMAIL instead (see Service.PromoteByEmail).
func (s *Store) CreateUser(ctx context.Context, email, passwordHash, displayName string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin create user: %w", err)
	}
	defer tx.Rollback()

	role := PlatformRoleUser
	if s.firstAccountAdmin {
		// Serialize concurrent first-signups: there is no row to lock on an
		// empty table, so two racing signups would both see zero users and
		// both become admins; a transaction-scoped advisory lock is the only
		// thing that orders them. Released at commit.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, bootstrapAdminLockKey); err != nil {
			return User{}, fmt.Errorf("bootstrap lock: %w", err)
		}
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
			return User{}, fmt.Errorf("count users: %w", err)
		}
		if existing == 0 {
			role = PlatformRoleAdmin
		}
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

// UserByEmail fetches a user by email (for login). The input is normalized
// (trimmed, lowercased) exactly like stored emails (migration 000046), so a
// lookup matches regardless of the caller's casing; the users.email unique
// constraint and the lower(email) index both guarantee one match.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.scanUser(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, normalizeEmail(email))
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
		phone    sql.NullString
	)
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &role, &disabled, &u.CreatedAt, &u.UpdatedAt, &phone); err != nil {
		return User{}, err
	}
	u.PlatformRole = PlatformRole(role)
	u.Phone = phone.String
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

// UserIDByTokenHash resolves a valid (unexpired) token to its user id and the
// token's expiry (the sliding-renewal check reads the remaining validity).
func (s *Store) UserIDByTokenHash(ctx context.Context, tokenHash string, now time.Time) (string, time.Time, error) {
	var userID string
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, expires_at FROM auth_tokens WHERE token_hash = $1 AND expires_at > $2`,
		tokenHash, now).Scan(&userID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrInvalidToken
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("resolve token: %w", err)
	}
	return userID, expiresAt, nil
}

// ExtendToken slides a token's expiry forward (sliding session renewal). The
// WHERE guard means it can only ever extend, never shorten: a concurrent
// request racing a renewal cannot clamp a freshly extended expiry.
func (s *Store) ExtendToken(ctx context.Context, tokenHash string, expiresAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE auth_tokens SET expires_at = $1 WHERE token_hash = $2 AND expires_at < $1`,
		expiresAt, tokenHash); err != nil {
		return fmt.Errorf("extend token: %w", err)
	}
	return nil
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

// UserByExternalIdentity resolves an OIDC (issuer, subject) pair to its platform
// account (enterprise-readiness P1-2). The pair comes from a verified id_token,
// never from client input, so it is the trusted external key. Returns
// ErrUserNotFound when no link exists yet (first SSO sign-in provisions one).
func (s *Store) UserByExternalIdentity(ctx context.Context, issuer, subject string) (User, error) {
	u, err := scanUserRow(s.db.QueryRowContext(ctx, `
		SELECT u.`+strings.ReplaceAll(userColumns, ", ", ", u.")+`
		FROM users u
		JOIN user_identities i ON i.user_id = u.id
		WHERE i.issuer = $1 AND i.subject = $2`, issuer, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("resolve external identity: %w", err)
	}
	return u, nil
}

// ProvisionExternalUser links an external identity to an account, creating the
// account on first sign-in. Provisioning strategy: if an account already holds
// the email the IdP asserts, link the identity to THAT account (the email is the
// enterprise's own join key — an employee who first signed up with a password and
// later signs in via SSO lands on one account, not two); otherwise create a fresh
// account. The fresh account's password_hash is set to an unusable sentinel so it
// can never authenticate by password (sign-in is via the IdP only), keeping the
// NOT NULL column satisfied. Returns the resolved account.
func (s *Store) ProvisionExternalUser(ctx context.Context, issuer, subject, email, displayName string) (User, error) {
	// IdP emails vary in casing; normalize before the merge lookup and storage
	// so an SSO account joins the account the same address signed up with.
	email = normalizeEmail(email)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin provision: %w", err)
	}
	defer tx.Rollback()

	// Serialize concurrent first-sign-ins for the same external identity so two
	// racing callbacks cannot both create the link/account.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "nowhere.sso:"+issuer+":"+subject); err != nil {
		return User{}, fmt.Errorf("provision lock: %w", err)
	}

	// Already linked? Return the existing account (idempotent retry / race loser).
	var linkedID string
	err = tx.QueryRowContext(ctx, `
		SELECT user_id FROM user_identities WHERE issuer = $1 AND subject = $2`, issuer, subject).Scan(&linkedID)
	if err == nil {
		if _, uerr := tx.ExecContext(ctx, `
			UPDATE user_identities SET last_login_at = now(), email = $3 WHERE issuer = $1 AND subject = $2`,
			issuer, subject, email); uerr != nil {
			return User{}, fmt.Errorf("touch identity: %w", uerr)
		}
		if err := tx.Commit(); err != nil {
			return User{}, fmt.Errorf("commit provision: %w", err)
		}
		return s.UserByID(ctx, linkedID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("lookup identity: %w", err)
	}

	// Not linked. Join onto an existing account with the same email when one
	// exists; otherwise create one (with the first-account bootstrap enabled,
	// the first account on an empty platform is admin, matching CreateUser so
	// an SSO-first deployment still gets an administrator).
	userID := ""
	if email != "" {
		_ = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&userID)
	}
	if userID == "" {
		role := PlatformRoleUser
		if s.firstAccountAdmin {
			var existing int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
				return User{}, fmt.Errorf("count users: %w", err)
			}
			if existing == 0 {
				role = PlatformRoleAdmin
			}
		}
		// Unusable-password sentinel: not a valid bcrypt hash, so bcrypt compare
		// always fails; satisfies NOT NULL without allowing password sign-in.
		err = tx.QueryRowContext(ctx, `
			INSERT INTO users (email, password_hash, display_name, platform_role)
			VALUES ($1, '!sso-no-password!', $2, $3) RETURNING id`,
			email, displayName, string(role)).Scan(&userID)
		if err != nil {
			return User{}, fmt.Errorf("create sso user: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_identities (user_id, issuer, subject, email) VALUES ($1, $2, $3, $4)`,
		userID, issuer, subject, email); err != nil {
		return User{}, fmt.Errorf("link identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit provision: %w", err)
	}
	return s.UserByID(ctx, userID)
}
