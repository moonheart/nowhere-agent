package identity

import "errors"

var (
	ErrUserExists       = errors.New("user already exists")
	ErrUserNotFound     = errors.New("user not found")
	ErrTeamNotFound     = errors.New("team not found")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidToken     = errors.New("invalid or expired token")
	ErrNotMember        = errors.New("user is not a member of the team")
)
