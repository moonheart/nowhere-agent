package identity

import "errors"

var (
	ErrUserExists         = errors.New("user already exists")
	ErrUserNotFound       = errors.New("user not found")
	ErrTeamNotFound       = errors.New("team not found")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrNotMember          = errors.New("user is not a member of the team")

	// ErrUserDisabled is returned when a disabled account tries to authenticate.
	ErrUserDisabled = errors.New("account is disabled")
	// ErrLastOwner is returned when an operation would leave a team with no
	// owner — removing or demoting the only one.
	ErrLastOwner = errors.New("team must retain at least one owner")
	// ErrInvalidRole is returned for a team role outside owner/admin/member.
	ErrInvalidRole = errors.New("invalid team role")
	// ErrSelfTarget is returned when an administrator aims a lock-out operation
	// (demote, disable, delete) at their own account.
	ErrSelfTarget = errors.New("operation cannot target your own account")
)
