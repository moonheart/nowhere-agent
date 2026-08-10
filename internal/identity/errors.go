package identity

// statusError is a sentinel error that also carries the HTTP status it maps to,
// so the platform's shared error boundary (httpx.StatusFor / Recovery) answers
// the right code for known failures instead of a blanket 500. The concrete
// statuses below mirror the mappings the identity HTTP handlers already used.
type statusError struct {
	msg    string
	status int
}

func (e statusError) Error() string   { return e.msg }
func (e statusError) HTTPStatus() int { return e.status }
func (e statusError) Is(target error) bool {
	other, ok := target.(statusError)
	return ok && other.msg == e.msg && other.status == e.status
}

// newStatus builds a sentinel carrying an HTTP status.
func newStatus(msg string, status int) statusError { return statusError{msg: msg, status: status} }

var (
	ErrUserExists         = newStatus("user already exists", 409)
	ErrUserNotFound       = newStatus("user not found", 404)
	ErrTeamNotFound       = newStatus("team not found", 404)
	ErrInvalidCredentials = newStatus("invalid credentials", 401)
	ErrInvalidToken       = newStatus("invalid or expired token", 401)
	ErrNotMember          = newStatus("user is not a member of the team", 403)

	// ErrUserDisabled is returned when a disabled account tries to authenticate.
	ErrUserDisabled = newStatus("account is disabled", 403)
	// ErrLastOwner is returned when an operation would leave a team with no
	// owner — removing or demoting the only one.
	ErrLastOwner = newStatus("team must retain at least one owner", 409)
	// ErrInvalidRole is returned for a team role outside owner/admin/member.
	ErrInvalidRole = newStatus("invalid team role", 400)
	// ErrSelfTarget is returned when an administrator aims a lock-out operation
	// (demote, disable, delete) at their own account.
	ErrSelfTarget = newStatus("operation cannot target your own account", 409)
	// ErrKeyNotFound is returned for a service-key id that matches nothing.
	ErrKeyNotFound = newStatus("service key not found", 404)
)
