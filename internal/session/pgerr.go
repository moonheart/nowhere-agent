package session

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// isActiveRunConflict reports whether err is a Postgres unique-violation, i.e. a
// concurrent writer won the race to create this session's active run (rejected
// by the single-active-run partial index or the (session_id, seq) constraint).
func isActiveRunConflict(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return strings.Contains(err.Error(), "duplicate key") ||
		strings.Contains(err.Error(), "unique constraint")
}
