// Package identity implements the identity-scope capability: users, teams,
// authentication, and the shared user/team/system scope model used by skills
// and memory for ownership, isolation, and access control.
package identity

import "time"

// Scope is the shared ownership/visibility model (design D8).
// Resources (skills, memories) are tagged with exactly one scope.
type Scope string

const (
	ScopeUser   Scope = "user"
	ScopeTeam   Scope = "team"
	ScopeSystem Scope = "system"
)

// ScopeRef identifies the owner of a scoped resource.
// Exactly one of UserID/TeamID is set for user/team scopes; neither for system.
type ScopeRef struct {
	Scope  Scope
	UserID string
	TeamID string
}

func UserScope(userID string) ScopeRef   { return ScopeRef{Scope: ScopeUser, UserID: userID} }
func TeamScope(teamID string) ScopeRef   { return ScopeRef{Scope: ScopeTeam, TeamID: teamID} }
func SystemScope() ScopeRef              { return ScopeRef{Scope: ScopeSystem} }

// PlatformRole is an account's authority over the PLATFORM — users, teams, and
// every scope. It is orthogonal to Role, which governs a single team's
// resources: an account can administer the platform while belonging to no team,
// and can own a team while being an ordinary platform account.
type PlatformRole string

const (
	// PlatformRoleUser is an ordinary account (default).
	PlatformRoleUser PlatformRole = "user"
	// PlatformRoleAdmin may administer accounts, teams, and all scopes.
	PlatformRoleAdmin PlatformRole = "admin"
)

// User is a platform account.
type User struct {
	ID           string
	Email        string
	DisplayName  string
	PasswordHash string
	// Phone is the normalized mobile number for phone/OTP accounts; "" for
	// email and SSO accounts.
	Phone string
	// PlatformRole is the account's platform-wide authority.
	PlatformRole PlatformRole
	// DisabledAt is set when the account is disabled: it keeps its data but
	// cannot authenticate, and its outstanding tokens are revoked. nil = enabled.
	DisabledAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// IsAdmin reports whether the account may administer the platform.
func (u User) IsAdmin() bool { return u.PlatformRole == PlatformRoleAdmin }

// Disabled reports whether the account is barred from authenticating.
func (u User) Disabled() bool { return u.DisabledAt != nil }

// Team is a grouping of users for shared resources.
type Team struct {
	ID        string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Role is a user's role within a team.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Membership links a user to a team with a role.
type Membership struct {
	TeamID    string
	UserID    string
	Role      Role
	CreatedAt time.Time
}

// Rank orders roles for "at least this role" checks: owner > admin > member.
// An unknown role ranks below member, so it never satisfies a check.
func (r Role) Rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleAdmin:
		return 2
	case RoleMember:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether r carries at least the authority of min.
func (r Role) AtLeast(min Role) bool { return r.Rank() >= min.Rank() }

// Valid reports whether r is one of the three defined roles.
func (r Role) Valid() bool { return r.Rank() > 0 }

// TeamMember is a team's membership joined to the member's account, as the
// member list renders it.
type TeamMember struct {
	UserID      string
	Email       string
	DisplayName string
	Role        Role
	// Disabled mirrors the account's state so a member list shows why a
	// disabled account cannot sign in.
	Disabled bool
	JoinedAt time.Time
}

// TeamWithRole is a team paired with the caller's role in it, as "my teams"
// renders it.
type TeamWithRole struct {
	Team Team
	Role Role
}

// TeamWithCount is a team paired with its membership size, as the platform-wide
// team list renders it.
type TeamWithCount struct {
	Team        Team
	MemberCount int
}

// Token is an issued auth credential (stored hashed).
type Token struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// ServiceKey is a long-lived programmatic credential (enterprise integration):
// issued by an admin, scoped to a user (inheriting that user's permissions),
// optionally non-expiring, revocable independently of the account's tokens.
type ServiceKey struct {
	ID         string
	Name       string
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	// RevokedAt is set when an admin revoked the key; revoked keys never
	// authenticate (soft delete keeps the audit trail).
	RevokedAt *time.Time
}
