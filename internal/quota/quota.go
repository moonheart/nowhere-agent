// Package quota enforces usage budgets and request rate limits
// (enterprise-readiness P1-1). The platform already RECORDS token usage; this
// package is what makes a limit bite. Two independent mechanisms:
//
//   - Budget: a monthly token cap per account / per team, checked at run
//     submit. Crossing it rejects the run with ErrBudgetExceeded (HTTP 429)
//     before any model call, so an over-budget caller cannot start spending.
//   - RateLimiter: a per-key token bucket over HTTP requests, smoothing bursts.
//
// Both are fail-open on infrastructure error: a budget DB that cannot be read,
// or a limiter that misbehaves, must not take chat down. Limits protect the
// budget, not the other way around — a caller hitting the limit sees 429, but
// the platform never 429s itself because the quota store hiccuped.
package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrBudgetExceeded is returned by Check when a scope has reached its monthly
// budget. The HTTP layer maps it to 429 Too Many Requests with a Retry-After
// hint; it is a sentinel so callers can errors.Is it rather than match strings.
var ErrBudgetExceeded = errors.New("monthly token budget exceeded")

// Scope identifies whose budget a row governs.
type Scope string

const (
	ScopeUser Scope = "user"
	ScopeTeam Scope = "team"
)

// Budget is one scope's monthly token limit.
type Budget struct {
	Scope         Scope     `json:"scope"`
	OwnerID       string    `json:"owner_id"`
	MonthlyTokens int64     `json:"monthly_tokens"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Store persists budgets in Postgres.
type Store struct {
	db *sql.DB
}

// NewStore creates a Postgres-backed budget store.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Set upserts a scope's monthly budget. monthlyTokens must be positive — a
// non-positive limit is meaningless (block everything) and almost always a
// mistake, so it is rejected rather than stored. To remove a limit, use Clear.
func (s *Store) Set(ctx context.Context, scope Scope, ownerID string, monthlyTokens int64) error {
	if monthlyTokens <= 0 {
		return fmt.Errorf("monthly budget must be positive, got %d", monthlyTokens)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_budgets (scope, owner_id, monthly_tokens, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (scope, owner_id)
		DO UPDATE SET monthly_tokens = EXCLUDED.monthly_tokens, updated_at = now()`,
		string(scope), ownerID, monthlyTokens)
	if err != nil {
		return fmt.Errorf("set budget: %w", err)
	}
	return nil
}

// Get returns a scope's budget, or false when none is set (no limit).
func (s *Store) Get(ctx context.Context, scope Scope, ownerID string) (Budget, bool, error) {
	var b Budget
	err := s.db.QueryRowContext(ctx, `
		SELECT scope, owner_id, monthly_tokens, updated_at
		FROM usage_budgets WHERE scope = $1 AND owner_id = $2`,
		string(scope), ownerID).
		Scan(&b.Scope, &b.OwnerID, &b.MonthlyTokens, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Budget{}, false, nil
	}
	if err != nil {
		return Budget{}, false, fmt.Errorf("get budget: %w", err)
	}
	return b, true, nil
}

// Clear removes a scope's budget (restores "no limit"). Returns false if none
// was set.
func (s *Store) Clear(ctx context.Context, scope Scope, ownerID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM usage_budgets WHERE scope = $1 AND owner_id = $2`,
		string(scope), ownerID)
	if err != nil {
		return false, fmt.Errorf("clear budget: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SpendFunc reports how many billable tokens (input + output) one scope has
// consumed in the current budget window. The server implements it over the
// usage store; narrowing to a func keeps enforcement decoupled from the
// read-side reporting package's types.
type SpendFunc func(ctx context.Context, ownerID string, from, to time.Time) (int64, error)

// BudgetReader reads one scope's budget. *Store satisfies it over Postgres; a
// test can satisfy it in memory so the checker's decision policy is exercisable
// without a database.
type BudgetReader interface {
	Get(ctx context.Context, scope Scope, ownerID string) (Budget, bool, error)
}

// Checker enforces budgets against current usage. Spend lookups are injected as
// functions, so the checker needs no knowledge of how usage is aggregated.
type Checker struct {
	budgets   BudgetReader
	userSpend SpendFunc
	teamSpend SpendFunc
	now       func() time.Time
}

// NewChecker wires a Checker. Either SpendFunc may be nil to skip that scope's
// enforcement (e.g. team checks off when attribution is unavailable).
func NewChecker(budgets BudgetReader, userSpend, teamSpend SpendFunc) *Checker {
	return &Checker{budgets: budgets, userSpend: userSpend, teamSpend: teamSpend,
		now: func() time.Time { return time.Now().UTC() }}
}

// SetClock overrides the checker's clock (tests).
func (c *Checker) SetClock(now func() time.Time) {
	if now != nil {
		c.now = now
	}
}

// monthWindow returns the current calendar month [start, next): the window a
// monthly budget governs. Calendar months, not a rolling 30 days, because that
// is how finance and the usage console both reckon a "month" (中国企业对账按月结).
func (c *Checker) monthWindow() (time.Time, time.Time) {
	now := c.now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// Check reports whether a run by userID (billed to teamID, or the platform when
// teamID is "") may proceed under the applicable budgets. It returns
// ErrBudgetExceeded when the user's own budget OR the billing team's budget is
// already met this month. A budget is "met" when current-month spend >= limit:
// the check is at submit, before this run spends anything, so an exactly-full
// budget blocks the next run.
//
// Fail-open: a budget read or a spend query that errors is treated as "within
// budget". Enforcement must never be the reason chat is down.
func (c *Checker) Check(ctx context.Context, userID, teamID string) error {
	if c.budgets == nil {
		return nil // no enforcement wired
	}
	from, to := c.monthWindow()

	if c.userSpend != nil {
		if err := c.checkScope(ctx, ScopeUser, userID, c.userSpend, from, to); err != nil {
			return err
		}
	}
	if teamID != "" && c.teamSpend != nil {
		if err := c.checkScope(ctx, ScopeTeam, teamID, c.teamSpend, from, to); err != nil {
			return err
		}
	}
	return nil
}

// checkScope applies one scope's budget. A missing budget is no limit; an error
// reading budget or spend is fail-open (nil).
func (c *Checker) checkScope(ctx context.Context, scope Scope, ownerID string, spend SpendFunc, from, to time.Time) error {
	b, ok, err := c.budgets.Get(ctx, scope, ownerID)
	if err != nil || !ok {
		return nil
	}
	used, err := spend(ctx, ownerID, from, to)
	if err != nil {
		return nil
	}
	if used >= b.MonthlyTokens {
		return fmt.Errorf("%w: %s %s used %d of %d tokens this month",
			ErrBudgetExceeded, scope, ownerID, used, b.MonthlyTokens)
	}
	return nil
}
