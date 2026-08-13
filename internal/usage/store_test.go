package usage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func testDSN() string {
	if v := os.Getenv("USAGE_PG_TEST_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
}

func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("no postgres reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fixture builds a user with a session, so runs can be attached to it.
type fixture struct {
	db        *sql.DB
	UserID    string
	SessionID string
	seq       int
}

func newFixture(t *testing.T, db *sql.DB) *fixture {
	t.Helper()
	f := &fixture{db: db}
	err := db.QueryRow(`
		INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		"usg-"+randSuffix()+"@test.dev").Scan(&f.UserID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, f.UserID) })

	if err := db.QueryRow(`
		INSERT INTO sessions (user_id, title) VALUES ($1, 'usage test') RETURNING id`,
		f.UserID).Scan(&f.SessionID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return f
}

// addRun inserts a run with the given usage counters at a given time. Passing
// nil for a counter stores NULL, which is what a run that never reached the
// model looks like.
func (f *fixture) addRun(t *testing.T, at time.Time, in, out, cacheRead, cacheWrite *int) {
	t.Helper()
	f.addRunAttr(t, at, "", "", in, out, cacheRead, cacheWrite)
}

// addRunAttr inserts a run stamped with a team/model attribution (P1-3). An
// empty teamID stores NULL (platform-billed / legacy), matching the column.
func (f *fixture) addRunAttr(t *testing.T, at time.Time, teamID, model string, in, out, cacheRead, cacheWrite *int) {
	t.Helper()
	f.seq++
	_, err := f.db.Exec(`
		INSERT INTO runs (session_id, seq, status, created_at, team_id, model, usage_input, usage_output, usage_cache_read, usage_cache_write)
		VALUES ($1, $2, 'done', $3, NULLIF($4,'')::uuid, NULLIF($5,''), $6, $7, $8, $9)`,
		f.SessionID, f.seq, at, teamID, model, npt(in), npt(out), npt(cacheRead), npt(cacheWrite))
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
}

func npt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func ip(v int) *int { return &v }

// joinTeam creates a team, puts the fixture's user in it, and returns its id.
func (f *fixture) joinTeam(t *testing.T) string {
	t.Helper()
	var teamID string
	if err := f.db.QueryRow(`INSERT INTO teams (name) VALUES ($1) RETURNING id`, "usg-"+randSuffix()).Scan(&teamID); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { f.db.Exec(`DELETE FROM teams WHERE id = $1`, teamID) })
	if _, err := f.db.Exec(`
		INSERT INTO team_memberships (team_id, user_id, role) VALUES ($1, $2, 'owner')`,
		teamID, f.UserID); err != nil {
		t.Fatalf("add membership: %v", err)
	}
	return teamID
}

// windowAround returns a range tight enough to exclude other tests' rows.
func windowAround(at time.Time) Range {
	return Range{From: at.Add(-time.Hour), To: at.Add(time.Hour)}
}

func TestForUserSumsRuns(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	at := time.Now()

	f.addRun(t, at, ip(100), ip(20), ip(5), ip(1))
	f.addRun(t, at, ip(200), ip(30), ip(0), ip(0))

	got, err := store.ForUser(context.Background(), f.UserID, windowAround(at))
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	want := Tokens{Input: 300, Output: 50, CacheRead: 5, CacheWrite: 1, Runs: 2}
	if got != want {
		t.Errorf("ForUser = %+v, want %+v", got, want)
	}
	if got.Total() != 350 {
		t.Errorf("Total() = %d, want 350 (input+output only)", got.Total())
	}
}

// A run that failed before its first model call has NULL counters. It must
// contribute zero and still be counted as a run, not turn the whole sum NULL.
func TestRunsWithoutRecordedUsageContributeZero(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	at := time.Now()

	f.addRun(t, at, nil, nil, nil, nil)
	f.addRun(t, at, ip(10), ip(2), nil, nil)

	got, err := store.ForUser(context.Background(), f.UserID, windowAround(at))
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	want := Tokens{Input: 10, Output: 2, Runs: 2}
	if got != want {
		t.Errorf("ForUser = %+v, want %+v", got, want)
	}
}

func TestForUserWithNoRunsIsZeroNotError(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)

	got, err := store.ForUser(context.Background(), f.UserID, windowAround(time.Now()))
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if (got != Tokens{}) {
		t.Errorf("ForUser with no runs = %+v, want zero", got)
	}
}

func TestRangeExcludesRunsOutsideIt(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)

	now := time.Now()
	old := now.Add(-72 * time.Hour)
	f.addRun(t, old, ip(999), ip(999), nil, nil)
	f.addRun(t, now, ip(10), ip(1), nil, nil)

	got, err := store.ForUser(context.Background(), f.UserID, windowAround(now))
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if got.Input != 10 || got.Runs != 1 {
		t.Errorf("ForUser = %+v, want only the in-range run (input 10, runs 1)", got)
	}
}

func TestForTeamSumsItsMembers(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	teamID := f.joinTeam(t)
	at := time.Now()

	f.addRun(t, at, ip(70), ip(7), nil, nil)

	got, err := store.ForTeam(context.Background(), teamID, windowAround(at))
	if err != nil {
		t.Fatalf("ForTeam: %v", err)
	}
	if got.Input != 70 || got.Output != 7 || got.Runs != 1 {
		t.Errorf("ForTeam = %+v, want the member's usage", got)
	}
}

// The documented approximation: a member of two teams counts toward both, so
// the team figures can sum above the platform total. This test exists so the
// behavior is pinned rather than surprising.
func TestMemberOfTwoTeamsCountsTowardEach(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	teamA := f.joinTeam(t)
	teamB := f.joinTeam(t)
	at := time.Now()

	f.addRun(t, at, ip(50), ip(5), nil, nil)

	rng := windowAround(at)
	a, err := store.ForTeam(context.Background(), teamA, rng)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.ForTeam(context.Background(), teamB, rng)
	if err != nil {
		t.Fatal(err)
	}
	if a.Input != 50 || b.Input != 50 {
		t.Errorf("teams saw %d and %d input tokens, want 50 each (the documented overlap)", a.Input, b.Input)
	}
}

func TestByUserRanksAndLabels(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	at := time.Now()

	light := newFixture(t, db)
	heavy := newFixture(t, db)
	light.addRun(t, at, ip(10), ip(1), nil, nil)
	heavy.addRun(t, at, ip(1000), ip(100), nil, nil)

	rows, err := store.ByUser(context.Background(), windowAround(at), 100)
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}

	var heavyIdx, lightIdx = -1, -1
	for i, r := range rows {
		switch r.ID {
		case heavy.UserID:
			heavyIdx = i
			if r.Label == "" {
				t.Error("row label (email) is empty")
			}
		case light.UserID:
			lightIdx = i
		}
	}
	if heavyIdx < 0 || lightIdx < 0 {
		t.Fatalf("both users should appear; got heavy=%d light=%d in %d rows", heavyIdx, lightIdx, len(rows))
	}
	if heavyIdx > lightIdx {
		t.Error("rows are not ordered by token total descending")
	}
}

func TestByTeamGroupsByTeam(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	teamID := f.joinTeam(t)
	at := time.Now()
	f.addRun(t, at, ip(42), ip(4), nil, nil)

	rows, err := store.ByTeam(context.Background(), windowAround(at), 100)
	if err != nil {
		t.Fatalf("ByTeam: %v", err)
	}
	for _, r := range rows {
		if r.ID == teamID {
			if r.Tokens.Input != 42 {
				t.Errorf("team row input = %d, want 42", r.Tokens.Input)
			}
			if r.Label == "" {
				t.Error("team row has no name")
			}
			return
		}
	}
	t.Errorf("team %s missing from %d rows", teamID, len(rows))
}

func TestDailyForUserBucketsByDay(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)

	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)
	f.addRun(t, yesterday, ip(10), ip(1), nil, nil)
	f.addRun(t, now, ip(20), ip(2), nil, nil)
	f.addRun(t, now, ip(5), ip(0), nil, nil)

	rows, err := store.DailyForUser(context.Background(), f.UserID,
		Range{From: yesterday.Add(-time.Hour), To: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("DailyForUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d daily rows, want 2", len(rows))
	}
	if rows[0].ID >= rows[1].ID {
		t.Errorf("daily rows are not oldest-first: %q then %q", rows[0].ID, rows[1].ID)
	}
	if rows[0].Tokens.Input != 10 {
		t.Errorf("first day input = %d, want 10", rows[0].Tokens.Input)
	}
	if rows[1].Tokens.Input != 25 || rows[1].Tokens.Runs != 2 {
		t.Errorf("second day = %+v, want input 25 over 2 runs", rows[1].Tokens)
	}
}

// TestDailyBucketsAreUTCDays pins the bucketing timezone: daily buckets must be
// UTC days regardless of the connection's TimeZone. The store reads through a
// pool pinned to America/New_York, where a run at 2026-03-10 00:30 UTC is still
// 2026-03-09 — date_trunc over a timestamptz would split it into the wrong
// bucket (and drift from the budget's UTC month window) without the explicit
// AT TIME ZONE 'UTC'.
func TestDailyBucketsAreUTCDays(t *testing.T) {
	tzDB, err := sql.Open("pgx", testDSN()+"&timezone=America/New_York")
	if err != nil {
		t.Fatalf("open tz-pinned db: %v", err)
	}
	t.Cleanup(func() { tzDB.Close() })
	f := newFixture(t, tzDB)
	store := NewStore(tzDB)

	// 00:30 UTC on March 10 — still March 9 in New York.
	at := time.Date(2026, 3, 10, 0, 30, 0, 0, time.UTC)
	f.addRun(t, at, ip(7), ip(1), nil, nil)

	rows, err := store.DailyForUser(context.Background(), f.UserID,
		Range{From: at.Add(-time.Hour), To: at.Add(time.Hour)})
	if err != nil {
		t.Fatalf("DailyForUser: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].ID != "2026-03-10" {
		t.Fatalf("bucket = %q, want the UTC day 2026-03-10 (connection tz would say 2026-03-09)", rows[0].ID)
	}
	if rows[0].Tokens.Input != 7 {
		t.Errorf("bucket input = %d, want 7", rows[0].Tokens.Input)
	}
}

func TestTotalsCoversAllUsers(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	at := time.Now()

	a := newFixture(t, db)
	b := newFixture(t, db)
	a.addRun(t, at, ip(11), ip(1), nil, nil)
	b.addRun(t, at, ip(22), ip(2), nil, nil)

	got, err := store.Totals(context.Background(), windowAround(at))
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	// Other tests may share the window, so assert a lower bound rather than
	// equality — the point is that both users are included.
	if got.Input < 33 || got.Runs < 2 {
		t.Errorf("Totals = %+v, want at least input 33 over 2 runs", got)
	}
}

func TestRangeBoundsDefaults(t *testing.T) {
	// A zero range must mean "everything", not "nothing" — an unset filter in
	// the UI should not silently report zero usage.
	from, to := Range{}.bounds()
	if !from.Before(time.Now()) {
		t.Errorf("zero From = %v, want a lower bound in the past", from)
	}
	if !to.After(time.Now()) {
		t.Errorf("zero To = %v, want an upper bound in the future", to)
	}
}

func TestTeamOverlapNoteIsStated(t *testing.T) {
	// The note is the only place the approximation is disclosed to a caller;
	// an empty constant would silently drop that disclosure from every report.
	if TeamOverlapNote == "" {
		t.Fatal("TeamOverlapNote must explain the team-attribution approximation")
	}
}

// Since P1-3 a run stamps its attributing team. A user in two teams whose runs
// are stamped to ONE team must count only toward that team — the exact
// attribution that replaces the membership approximation.
func TestStampedRunCountsOnlyTowardItsTeam(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	teamA := f.joinTeam(t)
	teamB := f.joinTeam(t)
	at := time.Now()

	// The run was billed to team A's key even though the user is also in B.
	f.addRunAttr(t, at, teamA, "m1", ip(50), ip(5), nil, nil)
	rng := windowAround(at)

	a, err := store.ForTeam(context.Background(), teamA, rng)
	if err != nil {
		t.Fatal(err)
	}
	if a.Input != 50 {
		t.Errorf("team A input = %d, want 50 (its stamped run)", a.Input)
	}
	b, err := store.ForTeam(context.Background(), teamB, rng)
	if err != nil {
		t.Fatal(err)
	}
	if b.Input != 0 || b.Runs != 0 {
		t.Errorf("team B = %+v, want zero — the run was attributed to A, not shared by membership", b)
	}
}

// A stamped run must NOT also be re-attributed by the legacy membership
// fallback, or it would double-count: once via team_id, once via membership.
func TestStampedRunNotDoubleCountedWithMembership(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	teamID := f.joinTeam(t)
	at := time.Now()

	// Stamped to the team AND a current member — must sum to 100, not 200.
	f.addRunAttr(t, at, teamID, "m1", ip(100), ip(10), nil, nil)

	got, err := store.ForTeam(context.Background(), teamID, windowAround(at))
	if err != nil {
		t.Fatal(err)
	}
	if got.Input != 100 || got.Runs != 1 {
		t.Errorf("ForTeam = %+v, want exactly 100 over 1 run (no double count)", got)
	}
}

// ByModel is the cost-accounting read: usage grouped by the recorded model,
// with unrecorded models (legacy rows) in their own bucket.
func TestByModelGroupsAndLabels(t *testing.T) {
	db := pgTestDB(t)
	f := newFixture(t, db)
	store := NewStore(db)
	at := time.Now()

	// Model names unique to this test: ByModel aggregates the WHOLE table (it
	// has no user/team dimension), so a fixed name like "deepseek-v4-flash"
	// deterministically collides with real platform runs whose model lands in
	// the ±1h window. A random suffix keeps the buckets self-contained — the
	// grouping/label behavior is pinned without depending on what the shared
	// dev database did in the last hour.
	m1 := "usg-" + randSuffix() + "-flash"
	m2 := "usg-" + randSuffix() + "-opus"

	f.addRunAttr(t, at, "", m1, ip(100), ip(10), nil, nil)
	f.addRunAttr(t, at, "", m1, ip(50), ip(5), nil, nil)
	f.addRunAttr(t, at, "", m2, ip(7), ip(3), nil, nil)

	rows, err := store.ByModel(context.Background(), windowAround(at), 100)
	if err != nil {
		t.Fatalf("ByModel: %v", err)
	}
	byModel := map[string]Tokens{}
	for _, r := range rows {
		byModel[r.ID] = r.Tokens
	}
	if got := byModel[m1]; got.Input != 150 || got.Runs != 2 {
		t.Errorf("%s = %+v, want input 150 over 2 runs", m1, got)
	}
	if got := byModel[m2]; got.Input != 7 || got.Runs != 1 {
		t.Errorf("%s = %+v, want input 7 over 1 run", m2, got)
	}
}
