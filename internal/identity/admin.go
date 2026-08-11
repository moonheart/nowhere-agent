package identity

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// This file holds the operations the management console drives (admin-console).
// The invariants that must not be bypassed live here rather than in the HTTP
// layer, so a second caller cannot reintroduce a lock-out.

// ---- platform administration ----

// PromoteByEmail grants the platform-admin role to the account with this email.
// It is the bootstrap path for a deployment whose accounts predate the role
// (BOOTSTRAP_ADMIN_EMAIL) and the recovery path when no admin remains. It
// reports whether an account matched; an email nobody holds is not an error,
// so a stale configuration value cannot block startup.
func (s *Service) PromoteByEmail(ctx context.Context, email string) (bool, error) {
	if email == "" {
		return false, nil
	}
	return s.store.PromoteByEmail(ctx, email)
}

// CreateAccount registers an account on an administrator's behalf, with an
// initial password the holder is expected to change.
func (s *Service) CreateAccount(ctx context.Context, email, password, displayName string) (User, error) {
	return s.Signup(ctx, email, password, displayName)
}

// SetPlatformRole grants or revokes the platform-admin role. actorID is the
// administrator making the change: they may not revoke their own role, because
// an admin who demotes themselves on a single-admin platform locks everyone out
// of administration with no in-product way back.
func (s *Service) SetPlatformRole(ctx context.Context, actorID, userID string, role PlatformRole) error {
	if role != PlatformRoleAdmin && role != PlatformRoleUser {
		return fmt.Errorf("unknown platform role %q", role)
	}
	if actorID == userID && role != PlatformRoleAdmin {
		return ErrSelfTarget
	}
	return s.store.SetPlatformRole(ctx, userID, role)
}

// SetUserDisabled disables or re-enables an account, revoking its tokens when
// disabling. An administrator may not disable themselves.
func (s *Service) SetUserDisabled(ctx context.Context, actorID, userID string, disabled bool) error {
	if actorID == userID && disabled {
		return ErrSelfTarget
	}
	return s.store.SetUserDisabled(ctx, userID, disabled)
}

// DeleteAccount removes an account and everything cascading from it. An
// administrator may not delete themselves (that would strand the platform
// without an admin; an admin leaves via the self-service DeleteSelf path,
// which does not require anyone to remain).
func (s *Service) DeleteAccount(ctx context.Context, actorID, userID string) error {
	if actorID == userID {
		return ErrSelfTarget
	}
	return s.store.DeleteUser(ctx, userID)
}

// DeleteSelf removes the CALLER's own account and everything cascading from
// it (PIPL §47 erasure right). Unlike DeleteAccount it is not admin-restricted
// — self-service deletion is the account owner's right, exercised from
// DELETE /api/me after an explicit confirm. The account's tokens die with it.
func (s *Service) DeleteSelf(ctx context.Context, userID string) error {
	return s.store.DeleteUser(ctx, userID)
}

// ResetPassword sets an account's password without knowing the old one. It is
// the administrator's path; an account changing its own password goes through
// ChangePassword, which verifies the current one.
func (s *Service) ResetPassword(ctx context.Context, userID, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return s.store.SetPassword(ctx, userID, string(hash))
}

// ---- self service ----

// ChangePassword replaces the caller's password after verifying the current
// one. Every token the account holds is revoked, so a password changed because
// it leaked also ends the sessions opened with it.
func (s *Service) ChangePassword(ctx context.Context, userID, current, next string) error {
	if err := validatePassword(next); err != nil {
		return err
	}
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(current)) != nil {
		return ErrInvalidCredentials
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return s.store.SetPassword(ctx, userID, string(hash))
}

// UpdateDisplayName changes an account's display name.
func (s *Service) UpdateDisplayName(ctx context.Context, userID, displayName string) error {
	return s.store.UpdateDisplayName(ctx, userID, displayName)
}

// ListTokens returns the account's live sessions.
func (s *Service) ListTokens(ctx context.Context, userID string) ([]Token, error) {
	return s.store.ListTokens(ctx, userID, s.now())
}

// CurrentTokenID resolves the raw bearer token of the request to the id of the
// session row it belongs to, so a session list can mark "this device".
func (s *Service) CurrentTokenID(ctx context.Context, rawToken string) (string, error) {
	return s.store.TokenIDByHash(ctx, hashToken(rawToken))
}

// RevokeToken ends one of the account's sessions.
func (s *Service) RevokeToken(ctx context.Context, userID, tokenID string) error {
	return s.store.DeleteTokenByID(ctx, userID, tokenID)
}

// RevokeOtherTokens ends every session except the one making the request, and
// returns how many were ended.
func (s *Service) RevokeOtherTokens(ctx context.Context, userID, currentRawToken string) (int, error) {
	return s.store.DeleteTokensForUserExcept(ctx, userID, hashToken(currentRawToken))
}

// ---- teams and membership ----

// TeamsForUser returns the caller's teams with their role in each.
func (s *Service) TeamsForUser(ctx context.Context, userID string) ([]TeamWithRole, error) {
	return s.store.TeamsForUser(ctx, userID)
}

// RoleInTeam returns the user's role in a team and whether they are a member.
func (s *Service) RoleInTeam(ctx context.Context, teamID, userID string) (Role, bool, error) {
	return s.store.RoleInTeam(ctx, teamID, userID)
}

// ListMembers returns a team's members.
func (s *Service) ListMembers(ctx context.Context, teamID string) ([]TeamMember, error) {
	return s.store.ListMembers(ctx, teamID)
}

// AddMemberByEmail adds an existing account to a team at a role. The account
// must already exist: creating one as a side effect of an invitation would
// mean issuing a password nobody chose, and there is no email channel to send
// one over.
func (s *Service) AddMemberByEmail(ctx context.Context, teamID, email string, role Role) (TeamMember, error) {
	if !role.Valid() {
		return TeamMember{}, ErrInvalidRole
	}
	u, err := s.store.UserByEmail(ctx, email)
	if err != nil {
		return TeamMember{}, err
	}
	if _, err := s.store.TeamByID(ctx, teamID); err != nil {
		return TeamMember{}, err
	}
	if err := s.store.AddMember(ctx, teamID, u.ID, role); err != nil {
		return TeamMember{}, err
	}
	return TeamMember{
		UserID:      u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        role,
		Disabled:    u.Disabled(),
	}, nil
}

// ChangeMemberRole changes a member's role, refusing to demote a team's last
// owner. AddMember upserts on (team, user), so this doubles as the "already a
// member" path of an add.
func (s *Service) ChangeMemberRole(ctx context.Context, teamID, userID string, role Role) error {
	if !role.Valid() {
		return ErrInvalidRole
	}
	current, ok, err := s.store.RoleInTeam(ctx, teamID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	if current == RoleOwner && role != RoleOwner {
		owners, err := s.store.CountOwners(ctx, teamID)
		if err != nil {
			return err
		}
		if owners <= 1 {
			return ErrLastOwner
		}
	}
	return s.store.AddMember(ctx, teamID, userID, role)
}

// RemoveMember drops a membership, refusing to remove a team's last owner. It
// serves both "an administrator removes someone" and "a member leaves".
func (s *Service) RemoveMember(ctx context.Context, teamID, userID string) error {
	current, ok, err := s.store.RoleInTeam(ctx, teamID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	if current == RoleOwner {
		owners, err := s.store.CountOwners(ctx, teamID)
		if err != nil {
			return err
		}
		if owners <= 1 {
			return ErrLastOwner
		}
	}
	return s.store.RemoveMember(ctx, teamID, userID)
}

// RenameTeam changes a team's name.
func (s *Service) RenameTeam(ctx context.Context, teamID, name string) error {
	return s.store.UpdateTeamName(ctx, teamID, name)
}

// DeleteTeam removes a team and its memberships and provider keys.
func (s *Service) DeleteTeam(ctx context.Context, teamID string) error {
	return s.store.DeleteTeam(ctx, teamID)
}

// TeamByID fetches a team.
func (s *Service) TeamByID(ctx context.Context, teamID string) (Team, error) {
	return s.store.TeamByID(ctx, teamID)
}

// ---- listings ----

// ListUsers returns accounts matching q with the total for paging.
func (s *Service) ListUsers(ctx context.Context, q string, limit, offset int) ([]User, int, error) {
	return s.store.ListUsers(ctx, q, limit, offset)
}

// ListTeams returns teams matching q with the total for paging.
func (s *Service) ListTeams(ctx context.Context, q string, limit, offset int) ([]TeamWithCount, int, error) {
	return s.store.ListTeams(ctx, q, limit, offset)
}

// UserByID fetches an account.
func (s *Service) UserByID(ctx context.Context, userID string) (User, error) {
	return s.store.UserByID(ctx, userID)
}

// PlatformStats is the console landing view's summary.
type PlatformStats struct {
	Users  int
	Admins int
	Teams  int
}

// Stats returns platform-wide counts.
func (s *Service) Stats(ctx context.Context) (PlatformStats, error) {
	users, err := s.store.CountUsers(ctx)
	if err != nil {
		return PlatformStats{}, err
	}
	admins, err := s.store.CountAdmins(ctx)
	if err != nil {
		return PlatformStats{}, err
	}
	_, teams, err := s.store.ListTeams(ctx, "", 1, 0)
	if err != nil {
		return PlatformStats{}, err
	}
	return PlatformStats{Users: users, Admins: admins, Teams: teams}, nil
}

// IsNotFound reports whether err is one of the "the thing isn't there" errors,
// which the HTTP layer maps to 404 rather than 500.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrUserNotFound) ||
		errors.Is(err, ErrTeamNotFound) ||
		errors.Is(err, ErrNotMember) ||
		errors.Is(err, ErrKeyNotFound)
}
