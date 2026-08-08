package quota

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// memBudgets is an in-memory BudgetReader so the checker's decision policy is
// exercisable without Postgres. The *Store's SQL (upsert, positivity, clear) is
// covered separately in the PG-backed store_test.go.
type memBudgets map[string]Budget

func (m memBudgets) Get(_ context.Context, scope Scope, ownerID string) (Budget, bool, error) {
	b, ok := m[string(scope)+"|"+ownerID]
	return b, ok, nil
}

// errBudgets always errors, to exercise the checker's fail-open path.
type errBudgets struct{}

func (errBudgets) Get(context.Context, Scope, string) (Budget, bool, error) {
	return Budget{}, false, errors.New("db down")
}

func fixedNow() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) }

func newChecker(b BudgetReader, userSpend, teamSpend SpendFunc) *Checker {
	c := NewChecker(b, userSpend, teamSpend)
	c.SetClock(fixedNow)
	return c
}

func spendReturning(total int64, err error) SpendFunc {
	return func(context.Context, string, time.Time, time.Time) (int64, error) { return total, err }
}

func TestCheckNoStoreIsUnlimited(t *testing.T) {
	called := false
	spy := func(context.Context, string, time.Time, time.Time) (int64, error) { called = true; return 0, nil }
	c := newChecker(nil, spy, spy)
	if err := c.Check(context.Background(), "u", "team"); err != nil {
		t.Fatalf("no budget store should never block, got %v", err)
	}
	if called {
		t.Fatal("spend funcs must not run when no budget store is wired")
	}
}

func TestCheckNoBudgetRowIsUnlimited(t *testing.T) {
	c := newChecker(memBudgets{}, spendReturning(1<<40, nil), nil)
	if err := c.Check(context.Background(), "u", ""); err != nil {
		t.Fatalf("absence of a budget row means no limit, got %v", err)
	}
}

func TestCheckUnderBudgetPasses(t *testing.T) {
	b := memBudgets{"user|u": {Scope: ScopeUser, OwnerID: "u", MonthlyTokens: 1000}}
	c := newChecker(b, spendReturning(999, nil), nil)
	if err := c.Check(context.Background(), "u", ""); err != nil {
		t.Fatalf("999 < 1000 should pass, got %v", err)
	}
}

func TestCheckAtBudgetBlocks(t *testing.T) {
	// A budget is "met" when current-month spend >= limit: the check is at submit,
	// before this run spends, so an exactly-full budget blocks the next run.
	b := memBudgets{"user|u": {Scope: ScopeUser, OwnerID: "u", MonthlyTokens: 1000}}
	c := newChecker(b, spendReturning(1000, nil), nil)
	err := c.Check(context.Background(), "u", "")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("spend == budget should block with ErrBudgetExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "1000 of 1000") {
		t.Fatalf("error should name the usage and limit, got %q", err.Error())
	}
}

func TestCheckOverBudgetBlocks(t *testing.T) {
	b := memBudgets{"user|u": {Scope: ScopeUser, OwnerID: "u", MonthlyTokens: 1000}}
	c := newChecker(b, spendReturning(5000, nil), nil)
	if err := c.Check(context.Background(), "u", ""); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over budget should block, got %v", err)
	}
}

func TestCheckTeamScopeBlocks(t *testing.T) {
	// The billing team's budget blocks even when the user has no budget of their own.
	b := memBudgets{"team|t1": {Scope: ScopeTeam, OwnerID: "t1", MonthlyTokens: 100}}
	c := newChecker(b, spendReturning(0, nil), spendReturning(150, nil))
	if err := c.Check(context.Background(), "u", "t1"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("team over budget should block, got %v", err)
	}
}

func TestCheckUserBudgetBlocksBeforeTeam(t *testing.T) {
	b := memBudgets{
		"user|u": {Scope: ScopeUser, OwnerID: "u", MonthlyTokens: 10},
		"team|t": {Scope: ScopeTeam, OwnerID: "t", MonthlyTokens: 10},
	}
	c := newChecker(b, spendReturning(10, nil), spendReturning(10, nil))
	if err := c.Check(context.Background(), "u", "t"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("both over budget should block, got %v", err)
	}
}

func TestCheckEmptyTeamSkipsTeamScope(t *testing.T) {
	// teamID "" means platform-billed: there is no team budget to enforce, so only
	// the user scope applies.
	called := false
	teamSpend := func(context.Context, string, time.Time, time.Time) (int64, error) { called = true; return 0, nil }
	b := memBudgets{"team|": {Scope: ScopeTeam, OwnerID: "", MonthlyTokens: 1}}
	c := newChecker(b, nil, teamSpend)
	if err := c.Check(context.Background(), "u", ""); err != nil {
		t.Fatalf("empty team should not enforce a team budget, got %v", err)
	}
	if called {
		t.Fatal("team spend must not be queried for a platform-billed run")
	}
}

func TestCheckNilUserSpendSkipsUserScope(t *testing.T) {
	// With only a team spend func wired, the user budget is not consulted.
	b := memBudgets{"user|u": {Scope: ScopeUser, OwnerID: "u", MonthlyTokens: 1}}
	c := newChecker(b, nil, spendReturning(0, nil))
	if err := c.Check(context.Background(), "u", "t"); err != nil {
		t.Fatalf("nil user spend should skip user enforcement, got %v", err)
	}
}

func TestCheckFailOpenOnBudgetReadError(t *testing.T) {
	c := newChecker(errBudgets{}, spendReturning(1<<40, nil), nil)
	if err := c.Check(context.Background(), "u", ""); err != nil {
		t.Fatalf("budget read error must be fail-open, got %v", err)
	}
}

func TestCheckFailOpenOnSpendError(t *testing.T) {
	b := memBudgets{"user|u": {Scope: ScopeUser, OwnerID: "u", MonthlyTokens: 1}}
	c := newChecker(b, spendReturning(0, errors.New("usage db down")), nil)
	if err := c.Check(context.Background(), "u", ""); err != nil {
		t.Fatalf("spend query error must be fail-open, got %v", err)
	}
}

func TestMonthWindowIsCalendarMonth(t *testing.T) {
	c := newChecker(nil, nil, nil)
	from, to := c.monthWindow()
	wantFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("month window = [%v, %v), want [%v, %v)", from, to, wantFrom, wantTo)
	}
}

func TestMonthWindowRollsAcrossYear(t *testing.T) {
	c := NewChecker(nil, nil, nil)
	c.SetClock(func() time.Time { return time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC) })
	from, to := c.monthWindow()
	if from.Month() != time.December || to.Month() != time.January || to.Year() != 2027 {
		t.Fatalf("December window should roll to next January, got [%v, %v)", from, to)
	}
}
