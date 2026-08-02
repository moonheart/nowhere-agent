package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file holds the read and write paths the management console needs
// (admin-console): listing and mutating accounts, teams, memberships, and
// tokens. store.go keeps the authentication path it has always had.

// ---- accounts ----

// ListUsers returns accounts matching q (a case-insensitive substring of email
// or display name; empty matches all), ordered by creation time, together with
// the total number of matches for paging. limit <= 0 is treated as 50.
func (s *Store) ListUsers(ctx context.Context, q string, limit, offset int) ([]User, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	// An empty pattern still has to be a valid LIKE argument, so match-all is
	// expressed as '%%' rather than branching the query.
	pattern := "%" + strings.ToLower(q) + "%"

	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM users
		WHERE lower(email) LIKE $1 OR lower(display_name) LIKE $1`, pattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+userColumns+` FROM users
		WHERE lower(email) LIKE $1 OR lower(display_name) LIKE $1
		ORDER BY created_at, id
		LIMIT $2 OFFSET $3`, pattern, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := make([]User, 0, limit)
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, total, rows.Err()
}

// CountUsers returns the total number of accounts.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins returns the number of accounts holding the platform-admin role.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE platform_role = $1`, string(PlatformRoleAdmin)).Scan(&n)
	return n, err
}

// SetPlatformRole grants or revokes the platform-admin role.
func (s *Store) SetPlatformRole(ctx context.Context, userID string, role PlatformRole) error {
	return s.updateUser(ctx, `UPDATE users SET platform_role = $2, updated_at = now() WHERE id = $1`, userID, string(role))
}

// PromoteByEmail grants the platform-admin role to the account with this email.
// It reports whether an account was found; promoting an already-admin account
// is a no-op that still reports found, so callers can log idempotently.
func (s *Store) PromoteByEmail(ctx context.Context, email string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET platform_role = $2, updated_at = now() WHERE email = $1`,
		email, string(PlatformRoleAdmin))
	if err != nil {
		return false, fmt.Errorf("promote by email: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetUserDisabled disables or re-enables an account. Disabling also revokes the
// account's outstanding tokens in the same transaction, so sessions already
// established do not outlive the decision. Re-enabling restores the ability to
// authenticate; it does not restore revoked tokens.
func (s *Store) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin disable user: %w", err)
	}
	defer tx.Rollback()

	var at any
	if disabled {
		at = time.Now()
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = $2, updated_at = now() WHERE id = $1`, userID, at)
	if IsMalformedID(err) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("set user disabled: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUserNotFound
	}
	if disabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("revoke tokens on disable: %w", err)
		}
	}
	return tx.Commit()
}

// UpdateDisplayName changes an account's display name.
func (s *Store) UpdateDisplayName(ctx context.Context, userID, displayName string) error {
	return s.updateUser(ctx, `UPDATE users SET display_name = $2, updated_at = now() WHERE id = $1`, userID, displayName)
}

// SetPassword replaces an account's password hash and revokes every token it
// holds, so a reset or change ends sessions opened with the old password. The
// caller re-authenticates afterwards.
func (s *Store) SetPassword(ctx context.Context, userID, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set password: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, userID, passwordHash)
	if IsMalformedID(err) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUserNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("revoke tokens on password change: %w", err)
	}
	return tx.Commit()
}

// DeleteUser removes an account. Tokens, memberships, and sessions cascade.
func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	return s.updateUser(ctx, `DELETE FROM users WHERE id = $1`, userID)
}

// updateUser runs a single-row user statement and maps "no row" to
// ErrUserNotFound so handlers do not have to inspect RowsAffected themselves.
func (s *Store) updateUser(ctx context.Context, q, userID string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, append([]any{userID}, args...)...)
	if IsMalformedID(err) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ---- teams ----

// ListTeams returns teams matching q (case-insensitive substring of the name;
// empty matches all) with the total for paging, plus each team's member count.
func (s *Store) ListTeams(ctx context.Context, q string, limit, offset int) ([]TeamWithCount, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	pattern := "%" + strings.ToLower(q) + "%"

	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM teams WHERE lower(name) LIKE $1`, pattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count teams: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.name, t.created_at, t.updated_at,
		       (SELECT count(*) FROM team_memberships m WHERE m.team_id = t.id)
		FROM teams t
		WHERE lower(t.name) LIKE $1
		ORDER BY t.created_at, t.id
		LIMIT $2 OFFSET $3`, pattern, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list teams: %w", err)
	}
	defer rows.Close()

	out := make([]TeamWithCount, 0, limit)
	for rows.Next() {
		var tc TeamWithCount
		if err := rows.Scan(&tc.Team.ID, &tc.Team.Name, &tc.Team.CreatedAt, &tc.Team.UpdatedAt, &tc.MemberCount); err != nil {
			return nil, 0, fmt.Errorf("scan team: %w", err)
		}
		out = append(out, tc)
	}
	return out, total, rows.Err()
}

// TeamByID fetches a team. Returns ErrTeamNotFound when absent.
func (s *Store) TeamByID(ctx context.Context, teamID string) (Team, error) {
	var t Team
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, created_at, updated_at FROM teams WHERE id = $1`, teamID).
		Scan(&t.ID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) || IsMalformedID(err) {
		return Team{}, ErrTeamNotFound
	}
	if err != nil {
		return Team{}, fmt.Errorf("query team: %w", err)
	}
	return t, nil
}

// UpdateTeamName renames a team.
func (s *Store) UpdateTeamName(ctx context.Context, teamID, name string) error {
	return s.updateTeam(ctx, `UPDATE teams SET name = $2, updated_at = now() WHERE id = $1`, teamID, name)
}

// DeleteTeam removes a team. Memberships and team keys cascade.
func (s *Store) DeleteTeam(ctx context.Context, teamID string) error {
	return s.updateTeam(ctx, `DELETE FROM teams WHERE id = $1`, teamID)
}

func (s *Store) updateTeam(ctx context.Context, q, teamID string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, append([]any{teamID}, args...)...)
	if IsMalformedID(err) {
		return ErrTeamNotFound
	}
	if err != nil {
		return fmt.Errorf("update team: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTeamNotFound
	}
	return nil
}

// TeamsForUser returns the teams the user belongs to with their role in each.
func (s *Store) TeamsForUser(ctx context.Context, userID string) ([]TeamWithRole, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.name, t.created_at, t.updated_at, m.role
		FROM teams t
		JOIN team_memberships m ON m.team_id = t.id
		WHERE m.user_id = $1
		ORDER BY t.name, t.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("teams for user: %w", err)
	}
	defer rows.Close()

	var out []TeamWithRole
	for rows.Next() {
		var (
			tr   TeamWithRole
			role string
		)
		if err := rows.Scan(&tr.Team.ID, &tr.Team.Name, &tr.Team.CreatedAt, &tr.Team.UpdatedAt, &role); err != nil {
			return nil, fmt.Errorf("scan team with role: %w", err)
		}
		tr.Role = Role(role)
		out = append(out, tr)
	}
	return out, rows.Err()
}

// ---- memberships ----

// ListMembers returns a team's members joined to their accounts.
func (s *Store) ListMembers(ctx context.Context, teamID string) ([]TeamMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.display_name, m.role, u.disabled_at IS NOT NULL, m.created_at
		FROM team_memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1
		ORDER BY m.created_at, u.id`, teamID)
	if IsMalformedID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	var out []TeamMember
	for rows.Next() {
		var (
			m    TeamMember
			role string
		)
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &role, &m.Disabled, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// RoleInTeam returns the user's role in the team and whether they are a member.
func (s *Store) RoleInTeam(ctx context.Context, teamID, userID string) (Role, bool, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `
		SELECT role FROM team_memberships WHERE team_id = $1 AND user_id = $2`, teamID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) || IsMalformedID(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("role in team: %w", err)
	}
	return Role(role), true, nil
}

// CountOwners returns how many owners a team has. It backs the guard that a
// team never loses its last owner.
func (s *Store) CountOwners(ctx context.Context, teamID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM team_memberships WHERE team_id = $1 AND role = $2`,
		teamID, string(RoleOwner)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count owners: %w", err)
	}
	return n, nil
}

// RemoveMember drops a membership. Returns ErrNotMember when there was none.
func (s *Store) RemoveMember(ctx context.Context, teamID, userID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM team_memberships WHERE team_id = $1 AND user_id = $2`, teamID, userID)
	if IsMalformedID(err) {
		return ErrNotMember
	}
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotMember
	}
	return nil
}

// ---- tokens ----

// ListTokens returns a user's unexpired tokens, newest first. Token hashes are
// never returned — only the metadata a "your active sessions" list needs.
func (s *Store) ListTokens(ctx context.Context, userID string, now time.Time) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, expires_at, created_at
		FROM auth_tokens
		WHERE user_id = $1 AND expires_at > $2
		ORDER BY created_at DESC`, userID, now)
	if IsMalformedID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.UserID, &t.ExpiresAt, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenIDByHash resolves a token hash to its id, so a caller can identify which
// listed session is the one making the request.
func (s *Store) TokenIDByHash(ctx context.Context, tokenHash string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM auth_tokens WHERE token_hash = $1`, tokenHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("token by hash: %w", err)
	}
	return id, nil
}

// DeleteTokenByID revokes one of the user's tokens. Scoped by user_id so a
// caller can only revoke their own sessions, whatever id they pass.
func (s *Store) DeleteTokenByID(ctx context.Context, userID, tokenID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM auth_tokens WHERE id = $1 AND user_id = $2`, tokenID, userID)
	if IsMalformedID(err) {
		return ErrInvalidToken
	}
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalidToken
	}
	return nil
}

// DeleteTokensForUserExcept revokes every token the user holds except the one
// with keepHash — "sign out everywhere else". An empty keepHash revokes all.
func (s *Store) DeleteTokensForUserExcept(ctx context.Context, userID, keepHash string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM auth_tokens WHERE user_id = $1 AND token_hash <> $2`, userID, keepHash)
	if err != nil {
		return 0, fmt.Errorf("delete tokens: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
