package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

// These tests exercise the console's read and write paths against a real
// Postgres, skipping when none is reachable (the convention the rest of the
// repo uses). Most run against the shared development database with
// randomly-named rows; the bootstrap tests need a genuinely empty `users`
// table, so they provision a throwaway database of their own.

const defaultTestDSN = "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"

func testDSN() string {
	if v := os.Getenv("IDENTITY_PG_TEST_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return defaultTestDSN
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

// mkUser inserts a user directly (bypassing CreateUser, so the bootstrap rule
// does not interfere) and registers cleanup.
func mkUser(t *testing.T, db *sql.DB) User {
	t.Helper()
	email := "idt-" + randSuffix() + "@test.dev"
	var u User
	err := db.QueryRow(`
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, 'x', $2) RETURNING id, email, display_name`,
		email, "u-"+randSuffix()).Scan(&u.ID, &u.Email, &u.DisplayName)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })
	return u
}

// mkTeam creates a team owned by owner and registers cleanup.
func mkTeam(t *testing.T, s *Store, owner User) Team {
	t.Helper()
	team, err := s.CreateTeam(context.Background(), "team-"+randSuffix(), owner.ID)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { s.DeleteTeam(context.Background(), team.ID) })
	return team
}

// ---- bootstrap: the first account on an empty platform ----

// freshDB provisions an empty database with the schema applied, so "is this the
// first account?" can be asked truthfully. It skips when Postgres is absent and
// drops the database afterwards.
func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	admin, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Skipf("open db: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("no postgres reachable: %v", err)
	}

	name := "nowhere_idt_" + randSuffix()
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Skipf("cannot create test database (needs CREATEDB): %v", err)
	}

	dsn, err := swapDatabase(testDSN(), name)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		// Reconnect to drop: the connection above holds the database open.
		a, err := sql.Open("pgx", testDSN())
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})

	driver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://../../migrations", "postgres", driver)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}
	return db
}

// swapDatabase replaces the database name in a Postgres DSN.
func swapDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func TestCreateUserFirstAccountBecomesAdmin(t *testing.T) {
	db := freshDB(t)
	store := NewStore(db)
	ctx := context.Background()

	first, err := store.CreateUser(ctx, "first@test.dev", "hash", "First")
	if err != nil {
		t.Fatalf("create first user: %v", err)
	}
	if !first.IsAdmin() {
		t.Errorf("first account platform_role = %q, want admin", first.PlatformRole)
	}

	second, err := store.CreateUser(ctx, "second@test.dev", "hash", "Second")
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	if second.IsAdmin() {
		t.Errorf("second account platform_role = %q, want user", second.PlatformRole)
	}
}

// Two signups racing on an empty platform must not both become administrators:
// the advisory lock is the only thing ordering them, since there is no row to
// lock on an empty table.
func TestCreateUserConcurrentFirstAccountsYieldOneAdmin(t *testing.T) {
	db := freshDB(t)
	store := NewStore(db)

	const n = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		users []User
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := store.CreateUser(context.Background(), fmt.Sprintf("race%d@test.dev", i), "hash", "")
			if err != nil {
				return
			}
			mu.Lock()
			users = append(users, u)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(users) != n {
		t.Fatalf("created %d users, want %d", len(users), n)
	}
	admins := 0
	for _, u := range users {
		if u.IsAdmin() {
			admins++
		}
	}
	if admins != 1 {
		t.Errorf("%d accounts hold the admin role, want exactly 1", admins)
	}
}

func TestCreateUserOnPopulatedPlatformIsOrdinary(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	// The shared development database already has accounts; if it somehow does
	// not, this assertion would be about the bootstrap path instead.
	if n, err := store.CountUsers(context.Background()); err != nil || n == 0 {
		t.Skipf("shared database is empty (%d users, err %v); covered by the fresh-database test", n, err)
	}

	u, err := store.CreateUser(context.Background(), "ord-"+randSuffix()+"@test.dev", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })
	if u.IsAdmin() {
		t.Errorf("account created on a populated platform is admin, want user")
	}
}

// ---- promotion ----

func TestPromoteByEmail(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()
	u := mkUser(t, db)

	found, err := svc.PromoteByEmail(ctx, u.Email)
	if err != nil || !found {
		t.Fatalf("PromoteByEmail = %v, %v; want true, nil", found, err)
	}
	got, err := store.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsAdmin() {
		t.Fatalf("platform_role = %q, want admin", got.PlatformRole)
	}

	// Idempotent: applied on every startup, so a second pass must be a no-op
	// that still reports the account was found.
	found, err = svc.PromoteByEmail(ctx, u.Email)
	if err != nil || !found {
		t.Errorf("second PromoteByEmail = %v, %v; want true, nil", found, err)
	}
}

func TestPromoteByEmailUnknownAddressIsNotAnError(t *testing.T) {
	db := pgTestDB(t)
	svc := NewService(NewStore(db))
	found, err := svc.PromoteByEmail(context.Background(), "nobody-"+randSuffix()+"@test.dev")
	if err != nil {
		t.Fatalf("unknown email must not error (it would block startup): %v", err)
	}
	if found {
		t.Error("reported an account was promoted when none matched")
	}
}

// ---- disablement ----

func TestSetUserDisabledRevokesTokensAndBlocksAuth(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, "dis-"+randSuffix()+"@test.dev", mustHash(t, "pw"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })

	token, _, err := svc.Login(ctx, u.Email, "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("authenticate before disable: %v", err)
	}

	if err := store.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); err == nil {
		t.Error("token still authenticates after the account was disabled")
	}
	if _, _, err := svc.Login(ctx, u.Email, "pw"); !errors.Is(err, ErrUserDisabled) {
		t.Errorf("login as disabled account = %v, want ErrUserDisabled", err)
	}

	// Re-enabling restores login but not the revoked token.
	if err := store.SetUserDisabled(ctx, u.ID, false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if _, _, err := svc.Login(ctx, u.Email, "pw"); err != nil {
		t.Errorf("login after re-enable: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); err == nil {
		t.Error("revoked token authenticates again after re-enable")
	}
}

func TestSetUserDisabledUnknownUser(t *testing.T) {
	db := pgTestDB(t)
	err := NewStore(db).SetUserDisabled(context.Background(), "00000000-0000-0000-0000-000000000000", true)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("err = %v, want ErrUserNotFound", err)
	}
}

// ---- passwords ----

func TestChangePassword(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, "pw-"+randSuffix()+"@test.dev", mustHash(t, "old-pw"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })

	if err := svc.ChangePassword(ctx, u.ID, "wrong", "new-pw-1234"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current password = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := svc.Login(ctx, u.Email, "old-pw"); err != nil {
		t.Fatalf("old password should still work after a refused change: %v", err)
	}

	if err := svc.ChangePassword(ctx, u.ID, "old-pw", "new-pw-1234"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, _, err := svc.Login(ctx, u.Email, "old-pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("old password still authenticates after the change")
	}
	if _, _, err := svc.Login(ctx, u.Email, "new-pw-1234"); err != nil {
		t.Errorf("new password does not authenticate: %v", err)
	}
}

// A password change ends the sessions opened with the old one — otherwise
// changing a leaked password would leave the leak's sessions alive.
func TestChangePasswordRevokesExistingTokens(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, "pwr-"+randSuffix()+"@test.dev", mustHash(t, "old-pw"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })

	token, _, err := svc.Login(ctx, u.Email, "old-pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangePassword(ctx, u.ID, "old-pw", "new-pw-1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, token); err == nil {
		t.Error("a session opened with the old password survived the change")
	}
}

// ---- teams and membership ----

func TestLastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	owner := mkUser(t, db)
	team := mkTeam(t, store, owner)

	if err := svc.RemoveMember(ctx, team.ID, owner.ID); !errors.Is(err, ErrLastOwner) {
		t.Errorf("removing the last owner = %v, want ErrLastOwner", err)
	}
	if err := svc.ChangeMemberRole(ctx, team.ID, owner.ID, RoleAdmin); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demoting the last owner = %v, want ErrLastOwner", err)
	}
	// Unchanged.
	role, ok, err := store.RoleInTeam(ctx, team.ID, owner.ID)
	if err != nil || !ok || role != RoleOwner {
		t.Errorf("owner role after refused operations = %q, %v, %v; want owner", role, ok, err)
	}
}

func TestOwnerCanBeRemovedWhenAnotherRemains(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	a := mkUser(t, db)
	b := mkUser(t, db)
	team := mkTeam(t, store, a)
	if err := store.AddMember(ctx, team.ID, b.ID, RoleOwner); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveMember(ctx, team.ID, a.ID); err != nil {
		t.Fatalf("removing an owner with another present: %v", err)
	}
	if _, ok, _ := store.RoleInTeam(ctx, team.ID, a.ID); ok {
		t.Error("membership survived removal")
	}
	// And now b is the last owner, so b is protected.
	if err := svc.RemoveMember(ctx, team.ID, b.ID); !errors.Is(err, ErrLastOwner) {
		t.Errorf("removing the now-last owner = %v, want ErrLastOwner", err)
	}
}

func TestAddMemberByEmail(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	owner := mkUser(t, db)
	joiner := mkUser(t, db)
	team := mkTeam(t, store, owner)

	m, err := svc.AddMemberByEmail(ctx, team.ID, joiner.Email, RoleMember)
	if err != nil {
		t.Fatalf("AddMemberByEmail: %v", err)
	}
	if m.UserID != joiner.ID || m.Role != RoleMember {
		t.Errorf("member = %+v, want %s at member role", m, joiner.ID)
	}

	members, err := store.ListMembers(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("team has %d members, want 2", len(members))
	}
}

func TestAddMemberByEmailUnknownAccount(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	owner := mkUser(t, db)
	team := mkTeam(t, store, owner)

	_, err := svc.AddMemberByEmail(context.Background(), team.ID, "ghost-"+randSuffix()+"@test.dev", RoleMember)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("err = %v, want ErrUserNotFound", err)
	}
}

func TestChangeMemberRoleOnNonMember(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	owner := mkUser(t, db)
	outsider := mkUser(t, db)
	team := mkTeam(t, store, owner)

	if err := svc.ChangeMemberRole(context.Background(), team.ID, outsider.ID, RoleAdmin); !errors.Is(err, ErrNotMember) {
		t.Errorf("err = %v, want ErrNotMember", err)
	}
}

func TestTeamsForUserCarriesRole(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	owner := mkUser(t, db)
	member := mkUser(t, db)
	team := mkTeam(t, store, owner)
	if err := store.AddMember(ctx, team.ID, member.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}

	got, err := store.TeamsForUser(ctx, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Team.ID != team.ID || got[0].Role != RoleAdmin {
		t.Errorf("TeamsForUser = %+v, want one team %s at admin", got, team.ID)
	}
}

func TestRoleInTeamForNonMember(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	owner := mkUser(t, db)
	outsider := mkUser(t, db)
	team := mkTeam(t, store, owner)

	role, ok, err := store.RoleInTeam(context.Background(), team.ID, outsider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok || role != "" {
		t.Errorf("RoleInTeam for a non-member = %q, %v; want \"\", false", role, ok)
	}
}

// ---- listings ----

func TestListUsersSearchAndPaging(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	tag := randSuffix()
	for i := 0; i < 3; i++ {
		var id string
		email := fmt.Sprintf("lst-%s-%d@test.dev", tag, i)
		if err := db.QueryRow(`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	}

	users, total, err := store.ListUsers(ctx, tag, 2, 0)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(users) != 2 {
		t.Errorf("page size = %d, want 2", len(users))
	}

	page2, _, err := store.ListUsers(ctx, tag, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Errorf("second page = %d rows, want 1", len(page2))
	}
	if len(users) > 0 && len(page2) > 0 && users[0].ID == page2[0].ID {
		t.Error("paging returned the same row twice")
	}
}

func TestListUsersEmptyQueryMatchesAll(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	_, total, err := store.ListUsers(ctx, "", 1, 0)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	count, err := store.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != count {
		t.Errorf("empty-query total = %d, want the full count %d", total, count)
	}
}

func TestListTeamsCarriesMemberCount(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	owner := mkUser(t, db)
	member := mkUser(t, db)
	team := mkTeam(t, store, owner)
	if err := store.AddMember(ctx, team.ID, member.ID, RoleMember); err != nil {
		t.Fatal(err)
	}

	teams, _, err := store.ListTeams(ctx, team.Name, 10, 0)
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("found %d teams named %q, want 1", len(teams), team.Name)
	}
	if teams[0].MemberCount != 2 {
		t.Errorf("member count = %d, want 2", teams[0].MemberCount)
	}
}

func TestTeamByIDNotFound(t *testing.T) {
	db := pgTestDB(t)
	_, err := NewStore(db).TeamByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("err = %v, want ErrTeamNotFound", err)
	}
}

// ---- tokens ----

func TestListAndRevokeTokens(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, "tok-"+randSuffix()+"@test.dev", mustHash(t, "pw"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })

	first, _, err := svc.Login(ctx, u.Email, "pw")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.Login(ctx, u.Email, "pw")
	if err != nil {
		t.Fatal(err)
	}

	tokens, err := svc.ListTokens(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("listed %d tokens, want 2", len(tokens))
	}

	// Revoking everything but the current session leaves exactly that session.
	n, err := svc.RevokeOtherTokens(ctx, u.ID, second)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("revoked %d tokens, want 1", n)
	}
	if _, err := svc.Authenticate(ctx, first); err == nil {
		t.Error("the other session still authenticates")
	}
	if _, err := svc.Authenticate(ctx, second); err != nil {
		t.Errorf("the current session was revoked too: %v", err)
	}
}

// A caller must not be able to revoke somebody else's session by passing its id.
func TestRevokeTokenIsScopedToOwner(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)
	ctx := context.Background()

	victim, err := store.CreateUser(ctx, "vic-"+randSuffix()+"@test.dev", mustHash(t, "pw"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, victim.ID) })
	attacker := mkUser(t, db)

	victimToken, _, err := svc.Login(ctx, victim.Email, "pw")
	if err != nil {
		t.Fatal(err)
	}
	victimTokenID, err := svc.CurrentTokenID(ctx, victimToken)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.RevokeToken(ctx, attacker.ID, victimTokenID); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("cross-account revoke = %v, want ErrInvalidToken", err)
	}
	if _, err := svc.Authenticate(ctx, victimToken); err != nil {
		t.Errorf("victim's session was revoked by another account: %v", err)
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return string(h)
}
