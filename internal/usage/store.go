// Package usage reports token consumption (admin-console). It is the read side
// of the accounting the session runtime writes: `runs.usage_*` holds one row per
// run, and this package aggregates those rows for the management console.
//
// Attribution is by ACCOUNT, because that is what the write path records —
// a run points at a session, and a session at the user who owns it. Team
// figures are therefore reconstructed as the sum over the team's current
// members, which has two consequences worth stating plainly rather than
// discovering later:
//
//   - an account belonging to several teams counts toward each of them, so
//     summing every team can exceed the platform total
//   - a member who leaves a team takes their historical usage with them
//
// TeamOverlapNote states this, and the HTTP layer attaches it to every report
// grouped by team, so a caller cannot render team figures as an exact partition
// without ignoring the payload.
//
// Reports are in tokens only. There is no `runs.model` column, so per-model
// breakdown and cost estimation would have to guess which model produced a run;
// a made-up cost is worse than no cost.
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"nowhere-agent/internal/identity"
)

// Tokens is a set of token counters, summed over some group of runs.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	// Runs is how many runs contributed, so a caller can tell "no activity"
	// from "activity that reported no tokens".
	Runs int64 `json:"runs"`
}

// Total is the sum of the billable input and output counters. Cache reads and
// writes are reported separately because providers price them differently and
// folding them in would misstate both.
func (t Tokens) Total() int64 { return t.Input + t.Output }

// Row is one group's usage in a grouped report.
type Row struct {
	// ID is the group key: an account id, a team id, or a date (YYYY-MM-DD)
	// depending on the query.
	ID string `json:"id"`
	// Label is a human name for the group — email, team name, or the date.
	Label  string `json:"label"`
	Tokens Tokens `json:"tokens"`
}

// Range bounds a report. A zero From means "since the beginning"; a zero To
// means "up to now".
type Range struct {
	From time.Time
	To   time.Time
}

// bounds normalizes a Range into concrete SQL arguments.
func (r Range) bounds() (time.Time, time.Time) {
	from := r.From
	if from.IsZero() {
		// Far enough back to precede any run; avoids a NULL-handling branch in
		// every query.
		from = time.Unix(0, 0)
	}
	to := r.To
	if to.IsZero() {
		to = time.Now().Add(24 * time.Hour)
	}
	return from, to
}

// TeamOverlapNote explains the team-attribution approximation. It rides along
// on every report grouped by team.
const TeamOverlapNote = "Team figures sum their current members' usage: runs record the account that owns the session, not a team. An account in several teams counts toward each, so team totals can exceed the platform total, and a member who leaves takes their history with them."

// Store aggregates persisted run usage from Postgres.
type Store struct {
	db *sql.DB
}

// NewStore creates a Postgres-backed usage store.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// selectTokens is the shared projection. COALESCE matters: usage columns are
// NULL for runs that never reported counts (a run that failed before its first
// model call), and SUM over an all-NULL group is itself NULL.
const selectTokens = `
	COALESCE(SUM(r.usage_input), 0),
	COALESCE(SUM(r.usage_output), 0),
	COALESCE(SUM(r.usage_cache_read), 0),
	COALESCE(SUM(r.usage_cache_write), 0),
	COUNT(r.id)`

// orderByTotal ranks groups by billable tokens. It repeats the expression
// rather than ordering by output position, because `ORDER BY 3 + 4` is a
// constant expression in Postgres, not a reference to the third and fourth
// columns — it would silently order by nothing.
const orderByTotal = `COALESCE(SUM(r.usage_input), 0) + COALESCE(SUM(r.usage_output), 0) DESC`

// Totals returns platform-wide usage over the range.
func (s *Store) Totals(ctx context.Context, rng Range) (Tokens, error) {
	from, to := rng.bounds()
	var t Tokens
	err := s.db.QueryRowContext(ctx, `
		SELECT `+selectTokens+`
		FROM runs r
		WHERE r.created_at >= $1 AND r.created_at < $2`, from, to).
		Scan(&t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Runs)
	if err != nil {
		return Tokens{}, fmt.Errorf("usage totals: %w", err)
	}
	return t, nil
}

// ForUser returns one account's usage over the range.
func (s *Store) ForUser(ctx context.Context, userID string, rng Range) (Tokens, error) {
	from, to := rng.bounds()
	var t Tokens
	err := s.db.QueryRowContext(ctx, `
		SELECT `+selectTokens+`
		FROM runs r
		JOIN sessions s ON s.id = r.session_id
		WHERE s.user_id = $1 AND r.created_at >= $2 AND r.created_at < $3`,
		userID, from, to).
		Scan(&t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Runs)
	// A malformed id names no account, so it has no usage — reporting zero is
	// truthful and keeps a mistyped link off the error budget.
	if identity.IsMalformedID(err) {
		return Tokens{}, nil
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("usage for user: %w", err)
	}
	return t, nil
}

// ForTeam returns a team's usage over the range: the sum over its current
// members (see the package comment on what that approximation costs).
func (s *Store) ForTeam(ctx context.Context, teamID string, rng Range) (Tokens, error) {
	from, to := rng.bounds()
	var t Tokens
	err := s.db.QueryRowContext(ctx, `
		SELECT `+selectTokens+`
		FROM runs r
		JOIN sessions s ON s.id = r.session_id
		JOIN team_memberships m ON m.user_id = s.user_id
		WHERE m.team_id = $1 AND r.created_at >= $2 AND r.created_at < $3`,
		teamID, from, to).
		Scan(&t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Runs)
	if identity.IsMalformedID(err) {
		return Tokens{}, nil
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("usage for team: %w", err)
	}
	return t, nil
}

// ByUser returns per-account usage over the range, heaviest first. Accounts
// with no runs in the range are omitted rather than reported as zero rows.
func (s *Store) ByUser(ctx context.Context, rng Range, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 100
	}
	from, to := rng.bounds()
	return s.rows(ctx, `
		SELECT s.user_id, u.email, `+selectTokens+`
		FROM runs r
		JOIN sessions s ON s.id = r.session_id
		JOIN users u ON u.id = s.user_id
		WHERE r.created_at >= $1 AND r.created_at < $2
		GROUP BY s.user_id, u.email
		ORDER BY `+orderByTotal+`, u.email
		LIMIT $3`, from, to, limit)
}

// ByTeam returns per-team usage over the range, heaviest first.
func (s *Store) ByTeam(ctx context.Context, rng Range, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 100
	}
	from, to := rng.bounds()
	return s.rows(ctx, `
		SELECT t.id, t.name, `+selectTokens+`
		FROM runs r
		JOIN sessions s ON s.id = r.session_id
		JOIN team_memberships m ON m.user_id = s.user_id
		JOIN teams t ON t.id = m.team_id
		WHERE r.created_at >= $1 AND r.created_at < $2
		GROUP BY t.id, t.name
		ORDER BY `+orderByTotal+`, t.name
		LIMIT $3`, from, to, limit)
}

// DailyForUser returns an account's usage per day over the range, oldest first.
func (s *Store) DailyForUser(ctx context.Context, userID string, rng Range) ([]Row, error) {
	from, to := rng.bounds()
	return s.rows(ctx, `
		SELECT to_char(date_trunc('day', r.created_at), 'YYYY-MM-DD') AS day,
		       to_char(date_trunc('day', r.created_at), 'YYYY-MM-DD'), `+selectTokens+`
		FROM runs r
		JOIN sessions s ON s.id = r.session_id
		WHERE s.user_id = $3 AND r.created_at >= $1 AND r.created_at < $2
		GROUP BY day
		ORDER BY day`, from, to, userID)
}

// DailyForTeam returns a team's usage per day over the range, oldest first.
func (s *Store) DailyForTeam(ctx context.Context, teamID string, rng Range) ([]Row, error) {
	from, to := rng.bounds()
	return s.rows(ctx, `
		SELECT to_char(date_trunc('day', r.created_at), 'YYYY-MM-DD') AS day,
		       to_char(date_trunc('day', r.created_at), 'YYYY-MM-DD'), `+selectTokens+`
		FROM runs r
		JOIN sessions s ON s.id = r.session_id
		JOIN team_memberships m ON m.user_id = s.user_id
		WHERE m.team_id = $3 AND r.created_at >= $1 AND r.created_at < $2
		GROUP BY day
		ORDER BY day`, from, to, teamID)
}

// DailyTotals returns platform-wide usage per day over the range, oldest first.
func (s *Store) DailyTotals(ctx context.Context, rng Range) ([]Row, error) {
	from, to := rng.bounds()
	return s.rows(ctx, `
		SELECT to_char(date_trunc('day', r.created_at), 'YYYY-MM-DD') AS day,
		       to_char(date_trunc('day', r.created_at), 'YYYY-MM-DD'), `+selectTokens+`
		FROM runs r
		WHERE r.created_at >= $1 AND r.created_at < $2
		GROUP BY day
		ORDER BY day`, from, to)
}

// rows runs a grouped query whose projection is (id, label, tokens...).
func (s *Store) rows(ctx context.Context, q string, args ...any) ([]Row, error) {
	rs, err := s.db.QueryContext(ctx, q, args...)
	if identity.IsMalformedID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("usage query: %w", err)
	}
	defer rs.Close()

	var out []Row
	for rs.Next() {
		var r Row
		if err := rs.Scan(&r.ID, &r.Label,
			&r.Tokens.Input, &r.Tokens.Output, &r.Tokens.CacheRead, &r.Tokens.CacheWrite, &r.Tokens.Runs); err != nil {
			return nil, fmt.Errorf("scan usage row: %w", err)
		}
		out = append(out, r)
	}
	return out, rs.Err()
}
