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

// User is a platform account.
type User struct {
	ID           string
	Email        string
	DisplayName  string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

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

// Token is an issued auth credential (stored hashed).
type Token struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
	CreatedAt time.Time
}
