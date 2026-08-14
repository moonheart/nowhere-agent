package identity

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint violation.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// Fallback for wrapped/driver variations.
	return strings.Contains(err.Error(), "duplicate key") ||
		strings.Contains(err.Error(), "unique constraint")
}

// IsMalformedID reports whether err is Postgres refusing a value that is not a
// valid uuid (SQLSTATE 22P02, invalid_text_representation).
//
// Ids reach the stores straight from URL path segments, so any typo'd or probed
// link — /api/teams/not-a-uuid — lands here. Without this, the query errors and
// the caller answers 500, which is both wrong (the resource simply does not
// exist) and noisy (a mistyped link pages an operator). Callers map it to their
// own not-found error instead.
func IsMalformedID(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "22P02"
	}
	return strings.Contains(err.Error(), "invalid input syntax for type uuid")
}
