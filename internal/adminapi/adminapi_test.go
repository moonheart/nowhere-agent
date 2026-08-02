package adminapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/routing"
	"nowhere-agent/internal/usage"
)

// The console's whole job is to let the right person do the right thing, so
// these tests are mostly an authorization matrix: for each route, which of
// {outsider, member, team admin, team owner, platform admin} gets through.
// The handlers are wired against a real Postgres because the guards consult
// membership; the memory port is in-memory, since scope checking does not
// depend on storage.

func testDSN() string {
	if v := os.Getenv("ADMINAPI_PG_TEST_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// env is a wired console with a swappable caller.
type env struct {
	t     *testing.T
	db    *sql.DB
	store *identity.Store
	svc   *identity.Service
	mem   memory.Port
	mux   *http.ServeMux

	// actor is who the fake auth middleware presents as the caller. Tests
	// reassign it to walk the authorization matrix.
	actor identity.User
}

func newEnv(t *testing.T) *env {
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

	e := &env{t: t, db: db, store: identity.NewStore(db), mem: memory.NewMemPort()}
	e.svc = identity.NewService(e.store)

	h := NewHandler(e.svc, routing.NewPGKeyStore(db, "platform-key"), usage.NewStore(db), e.mem)
	e.mux = http.NewServeMux()
	// Stand-in for identity's RequireAuth: it puts the current actor on the
	// context exactly as the real middleware does, without needing a token.
	h.RegisterAuthed(e.mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), e.actor)))
		})
	})
	return e
}

// user creates an account with the given platform role.
func (e *env) user(role identity.PlatformRole) identity.User {
	e.t.Helper()
	var u identity.User
	email := "adm-" + randSuffix() + "@test.dev"
	err := e.db.QueryRow(`
		INSERT INTO users (email, password_hash, display_name, platform_role)
		VALUES ($1, 'x', $2, $3)
		RETURNING id, email, display_name, platform_role`,
		email, "u-"+randSuffix(), string(role)).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.PlatformRole)
	if err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	e.t.Cleanup(func() { e.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })
	return u
}

// team creates a team owned by owner.
func (e *env) team(owner identity.User) identity.Team {
	e.t.Helper()
	tm, err := e.store.CreateTeam(context.Background(), "team-"+randSuffix(), owner.ID)
	if err != nil {
		e.t.Fatalf("create team: %v", err)
	}
	e.t.Cleanup(func() { e.store.DeleteTeam(context.Background(), tm.ID) })
	return tm
}

func (e *env) join(tm identity.Team, u identity.User, role identity.Role) {
	e.t.Helper()
	if err := e.store.AddMember(context.Background(), tm.ID, u.ID, role); err != nil {
		e.t.Fatalf("add member: %v", err)
	}
}

// as runs a request with the given caller and returns the recorder.
func (e *env) as(u identity.User, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	e.actor = u

	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

// ---- platform tier ----

func TestPlatformRoutesRejectNonAdmins(t *testing.T) {
	e := newEnv(t)
	ordinary := e.user(identity.PlatformRoleUser)
	admin := e.user(identity.PlatformRoleAdmin)
	// A team owner is still an ordinary platform account: owning a team must
	// not confer platform authority.
	owner := e.user(identity.PlatformRoleUser)
	e.team(owner)

	routes := []struct{ method, path string }{
		{"GET", "/api/admin/stats"},
		{"GET", "/api/admin/users"},
		{"GET", "/api/admin/teams"},
		{"GET", "/api/admin/usage"},
		{"GET", "/api/admin/memories"},
	}
	for _, rt := range routes {
		for _, u := range []identity.User{ordinary, owner} {
			rec := e.as(u, rt.method, rt.path, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as non-admin = %d, want 403", rt.method, rt.path, rec.Code)
			}
		}
		if rec := e.as(admin, rt.method, rt.path, nil); rec.Code != http.StatusOK {
			t.Errorf("%s %s as admin = %d (%s), want 200", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminListsAndCreatesUsers(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	email := "made-" + randSuffix() + "@test.dev"
	rec := e.as(admin, "POST", "/api/admin/users", map[string]string{
		"email": email, "password": "correct-horse", "display_name": "Made",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	created := body["user"].(map[string]any)
	newID := created["id"].(string)
	t.Cleanup(func() { e.db.Exec(`DELETE FROM users WHERE id = $1`, newID) })

	if created["platform_role"] != "user" {
		t.Errorf("created account role = %v, want user", created["platform_role"])
	}

	rec = e.as(admin, "GET", "/api/admin/users?q="+email, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list users = %d", rec.Code)
	}
	list := decodeBody(t, rec)
	if total, _ := list["total"].(float64); total != 1 {
		t.Errorf("search for the new email found %v accounts, want 1", list["total"])
	}
}

func TestCreateUserRejectsShortPassword(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	rec := e.as(admin, "POST", "/api/admin/users", map[string]string{
		"email": "short-" + randSuffix() + "@test.dev", "password": "abc",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("short password = %d, want 400", rec.Code)
	}
}

func TestAdminGrantsAndRevokesPlatformRole(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	target := e.user(identity.PlatformRoleUser)

	rec := e.as(admin, "PATCH", "/api/admin/users/"+target.ID, map[string]string{"platform_role": "admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("grant = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["user"].(map[string]any)["platform_role"]; got != "admin" {
		t.Errorf("role after grant = %v, want admin", got)
	}

	// The promoted account can now reach platform routes.
	promoted, err := e.store.UserByID(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.as(promoted, "GET", "/api/admin/stats", nil); rec.Code != http.StatusOK {
		t.Errorf("promoted account on a platform route = %d, want 200", rec.Code)
	}

	rec = e.as(admin, "PATCH", "/api/admin/users/"+target.ID, map[string]string{"platform_role": "user"})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200", rec.Code)
	}
}

// An administrator must not be able to strand the platform without one.
func TestAdminCannotLockThemselvesOut(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	cases := []struct {
		name   string
		method string
		body   any
	}{
		{"self-demotion", "PATCH", map[string]string{"platform_role": "user"}},
		{"self-disable", "PATCH", map[string]bool{"disabled": true}},
		{"self-deletion", "DELETE", nil},
	}
	for _, c := range cases {
		rec := e.as(admin, c.method, "/api/admin/users/"+admin.ID, c.body)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s = %d (%s), want 409", c.name, rec.Code, rec.Body.String())
		}
	}

	// And the account is untouched.
	after, err := e.store.UserByID(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("account should still exist: %v", err)
	}
	if !after.IsAdmin() || after.Disabled() {
		t.Errorf("account changed despite refusals: role=%q disabled=%v", after.PlatformRole, after.Disabled())
	}
}

func TestAdminDisablesAnotherAccount(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	target := e.user(identity.PlatformRoleUser)

	rec := e.as(admin, "PATCH", "/api/admin/users/"+target.ID, map[string]bool{"disabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["user"].(map[string]any)["disabled"]; got != true {
		t.Errorf("disabled = %v, want true", got)
	}
}

func TestPlatformUsageGroupings(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	rec := e.as(admin, "GET", "/api/admin/usage?group_by=user", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group_by=user = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["group_by"]; got != "user" {
		t.Errorf("group_by = %v, want user", got)
	}

	rec = e.as(admin, "GET", "/api/admin/usage?group_by=team", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group_by=team = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	// The overlap caveat must travel with team-grouped numbers, or a reader
	// will take them for an exact partition.
	if body["approximate"] != true {
		t.Error("team-grouped usage is not flagged approximate")
	}
	if note, _ := body["note"].(string); note == "" {
		t.Error("team-grouped usage carries no explanation of the approximation")
	}
}

func TestAdminMemoriesScopeQuery(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	if rec := e.as(admin, "GET", "/api/admin/memories?scope=user", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("scope=user without user_id = %d, want 400", rec.Code)
	}
	if rec := e.as(admin, "GET", "/api/admin/memories?scope=team", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("scope=team without team_id = %d, want 400", rec.Code)
	}
	if rec := e.as(admin, "GET", "/api/admin/memories?scope=nonsense", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown scope = %d, want 400", rec.Code)
	}
	if rec := e.as(admin, "GET", "/api/admin/memories?scope=system", nil); rec.Code != http.StatusOK {
		t.Errorf("scope=system = %d, want 200", rec.Code)
	}
}

// ---- team tier ----

func TestTeamRoutesAuthorizationMatrix(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	teamAdmin := e.user(identity.PlatformRoleUser)
	member := e.user(identity.PlatformRoleUser)
	outsider := e.user(identity.PlatformRoleUser)
	platformAdmin := e.user(identity.PlatformRoleAdmin)

	tm := e.team(owner)
	e.join(tm, teamAdmin, identity.RoleAdmin)
	e.join(tm, member, identity.RoleMember)

	cases := []struct {
		name           string
		method, path   string
		body           any
		wantOutsider   int
		wantMember     int
		wantTeamAdmin  int
		wantPlatformOK bool
	}{
		{
			name: "read team", method: "GET", path: "/api/teams/" + tm.ID,
			wantOutsider: 404, wantMember: 200, wantTeamAdmin: 200, wantPlatformOK: true,
		},
		{
			name: "list members", method: "GET", path: "/api/teams/" + tm.ID + "/members",
			wantOutsider: 404, wantMember: 200, wantTeamAdmin: 200, wantPlatformOK: true,
		},
		{
			name: "read keys", method: "GET", path: "/api/teams/" + tm.ID + "/keys",
			wantOutsider: 404, wantMember: 403, wantTeamAdmin: 200, wantPlatformOK: true,
		},
		{
			name: "read usage", method: "GET", path: "/api/teams/" + tm.ID + "/usage",
			wantOutsider: 404, wantMember: 403, wantTeamAdmin: 200, wantPlatformOK: true,
		},
		{
			name: "read memories", method: "GET", path: "/api/teams/" + tm.ID + "/memories",
			wantOutsider: 404, wantMember: 200, wantTeamAdmin: 200, wantPlatformOK: true,
		},
		{
			name: "rename", method: "PATCH", path: "/api/teams/" + tm.ID, body: map[string]string{"name": "renamed-" + randSuffix()},
			wantOutsider: 404, wantMember: 403, wantTeamAdmin: 204, wantPlatformOK: true,
		},
	}

	for _, c := range cases {
		if rec := e.as(outsider, c.method, c.path, c.body); rec.Code != c.wantOutsider {
			t.Errorf("%s as outsider = %d, want %d", c.name, rec.Code, c.wantOutsider)
		}
		if rec := e.as(member, c.method, c.path, c.body); rec.Code != c.wantMember {
			t.Errorf("%s as member = %d, want %d", c.name, rec.Code, c.wantMember)
		}
		if rec := e.as(teamAdmin, c.method, c.path, c.body); rec.Code != c.wantTeamAdmin {
			t.Errorf("%s as team admin = %d (%s), want %d", c.name, rec.Code, rec.Body.String(), c.wantTeamAdmin)
		}
		if c.wantPlatformOK {
			rec := e.as(platformAdmin, c.method, c.path, c.body)
			if rec.Code >= 400 {
				t.Errorf("%s as platform admin (not a member) = %d (%s), want success", c.name, rec.Code, rec.Body.String())
			}
		}
	}
}

// A non-member must not be able to tell an existing team from a missing one.
func TestNonMemberCannotProbeTeamExistence(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	outsider := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	real := e.as(outsider, "GET", "/api/teams/"+tm.ID, nil)
	fake := e.as(outsider, "GET", "/api/teams/00000000-0000-0000-0000-000000000000", nil)

	if real.Code != http.StatusNotFound || fake.Code != http.StatusNotFound {
		t.Fatalf("existing team = %d, missing team = %d; both should be 404", real.Code, fake.Code)
	}
	if real.Body.String() != fake.Body.String() {
		t.Errorf("responses differ, leaking existence:\n existing: %s\n missing:  %s", real.Body.String(), fake.Body.String())
	}
}

func TestDeleteTeamRequiresOwner(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	teamAdmin := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	e.join(tm, teamAdmin, identity.RoleAdmin)

	if rec := e.as(teamAdmin, "DELETE", "/api/teams/"+tm.ID, nil); rec.Code != http.StatusForbidden {
		t.Errorf("team admin deleting a team = %d, want 403", rec.Code)
	}
	if rec := e.as(owner, "DELETE", "/api/teams/"+tm.ID, nil); rec.Code != http.StatusNoContent {
		t.Errorf("owner deleting a team = %d, want 204", rec.Code)
	}
}

func TestChangeMemberRoleRequiresOwner(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	teamAdmin := e.user(identity.PlatformRoleUser)
	member := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	e.join(tm, teamAdmin, identity.RoleAdmin)
	e.join(tm, member, identity.RoleMember)

	path := "/api/teams/" + tm.ID + "/members/" + member.ID
	if rec := e.as(teamAdmin, "PATCH", path, map[string]string{"role": "admin"}); rec.Code != http.StatusForbidden {
		t.Errorf("team admin changing a role = %d, want 403", rec.Code)
	}
	if rec := e.as(owner, "PATCH", path, map[string]string{"role": "admin"}); rec.Code != http.StatusNoContent {
		t.Errorf("owner changing a role = %d, want 204", rec.Code)
	}
}

// A team admin creating an owner would escalate past the role they hold.
func TestTeamAdminCannotMintAnOwner(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	teamAdmin := e.user(identity.PlatformRoleUser)
	joiner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	e.join(tm, teamAdmin, identity.RoleAdmin)

	body := map[string]string{"email": joiner.Email, "role": "owner"}
	if rec := e.as(teamAdmin, "POST", "/api/teams/"+tm.ID+"/members", body); rec.Code != http.StatusForbidden {
		t.Errorf("team admin adding an owner = %d, want 403", rec.Code)
	}
	// A team admin may still add ordinary members.
	body = map[string]string{"email": joiner.Email, "role": "member"}
	if rec := e.as(teamAdmin, "POST", "/api/teams/"+tm.ID+"/members", body); rec.Code != http.StatusCreated {
		t.Errorf("team admin adding a member = %d, want 201", rec.Code)
	}
	// The owner may.
	other := e.user(identity.PlatformRoleUser)
	body = map[string]string{"email": other.Email, "role": "owner"}
	if rec := e.as(owner, "POST", "/api/teams/"+tm.ID+"/members", body); rec.Code != http.StatusCreated {
		t.Errorf("owner adding an owner = %d, want 201", rec.Code)
	}
}

func TestAddMemberUnknownEmail(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	rec := e.as(owner, "POST", "/api/teams/"+tm.ID+"/members", map[string]string{
		"email": "ghost-" + randSuffix() + "@test.dev", "role": "member",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("adding an unknown email = %d, want 404", rec.Code)
	}
}

// Removal admits any member, because leaving is removing yourself — but only
// yourself.
func TestMemberMayLeaveButNotRemoveOthers(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	memberA := e.user(identity.PlatformRoleUser)
	memberB := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	e.join(tm, memberA, identity.RoleMember)
	e.join(tm, memberB, identity.RoleMember)

	if rec := e.as(memberA, "DELETE", "/api/teams/"+tm.ID+"/members/"+memberB.ID, nil); rec.Code != http.StatusForbidden {
		t.Errorf("member removing another member = %d, want 403", rec.Code)
	}
	if rec := e.as(memberA, "DELETE", "/api/teams/"+tm.ID+"/members/"+memberA.ID, nil); rec.Code != http.StatusNoContent {
		t.Errorf("member leaving = %d, want 204", rec.Code)
	}
	if _, stillMember, _ := e.store.RoleInTeam(context.Background(), tm.ID, memberA.ID); stillMember {
		t.Error("membership survived leaving")
	}
}

func TestLastOwnerCannotLeave(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	rec := e.as(owner, "DELETE", "/api/teams/"+tm.ID+"/members/"+owner.ID, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("last owner leaving = %d (%s), want 409", rec.Code, rec.Body.String())
	}
}

// ---- provider keys ----

func TestTeamKeysNeverReturnPlaintext(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	const secret = "sk-ant-do-not-echo-me-9999"

	rec := e.as(owner, "PUT", "/api/teams/"+tm.ID+"/keys/anthropic", map[string]string{"api_key": secret})
	if rec.Code != http.StatusOK {
		t.Fatalf("put key = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "do-not-echo-me") {
		t.Errorf("the write response echoed the secret: %s", rec.Body.String())
	}

	rec = e.as(owner, "GET", "/api/teams/"+tm.ID+"/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "do-not-echo-me") {
		t.Errorf("the listing returned the secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "9999") {
		t.Errorf("the listing has no masked fragment to tell keys apart: %s", rec.Body.String())
	}
}

func TestPutKeyRejectsUnknownProvider(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	rec := e.as(owner, "PUT", "/api/teams/"+tm.ID+"/keys/nonesuch", map[string]string{"api_key": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown provider = %d, want 400", rec.Code)
	}
}

func TestPutKeyRejectsEmptyValue(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	rec := e.as(owner, "PUT", "/api/teams/"+tm.ID+"/keys/anthropic", map[string]string{"api_key": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("blank key = %d, want 400", rec.Code)
	}
}

func TestDeleteMissingKeyIs404(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	rec := e.as(owner, "DELETE", "/api/teams/"+tm.ID+"/keys/openai", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleting an unconfigured provider = %d, want 404", rec.Code)
	}
}

// ---- memory scope enforcement ----

// Deprecate and Forget take a bare id, so the scope check is the only thing
// standing between a team admin and another team's memories.
func TestTeamAdminCannotReachAnotherTeamsMemory(t *testing.T) {
	e := newEnv(t)
	ownerA := e.user(identity.PlatformRoleUser)
	ownerB := e.user(identity.PlatformRoleUser)
	teamA := e.team(ownerA)
	teamB := e.team(ownerB)

	victim, err := e.mem.Store(context.Background(), memory.Memory{
		Scope: identity.TeamScope(teamB.ID), Kind: memory.KindFact, Content: "team B's secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	// ownerA aims at team B's memory through team A's route.
	rec := e.as(ownerA, "DELETE", "/api/teams/"+teamA.ID+"/memories/"+victim.ID, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-team delete = %d (%s), want 404", rec.Code, rec.Body.String())
	}
	if _, err := e.mem.GetByID(context.Background(), victim.ID); err != nil {
		t.Errorf("the other team's memory was deleted: %v", err)
	}

	// Through its own route it works.
	own, err := e.mem.Store(context.Background(), memory.Memory{
		Scope: identity.TeamScope(teamA.ID), Kind: memory.KindFact, Content: "team A's own",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.as(ownerA, "DELETE", "/api/teams/"+teamA.ID+"/memories/"+own.ID, nil); rec.Code != http.StatusNoContent {
		t.Errorf("deleting the team's own memory = %d (%s), want 204", rec.Code, rec.Body.String())
	}
}

func TestSelfMemoryDeletionIsScopedToTheCaller(t *testing.T) {
	e := newEnv(t)
	a := e.user(identity.PlatformRoleUser)
	b := e.user(identity.PlatformRoleUser)

	victim, err := e.mem.Store(context.Background(), memory.Memory{
		Scope: identity.UserScope(b.ID), Kind: memory.KindPreference, Content: "b's preference",
	})
	if err != nil {
		t.Fatal(err)
	}

	if rec := e.as(a, "DELETE", "/api/me/memories/"+victim.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting another account's memory = %d, want 404", rec.Code)
	}
	if _, err := e.mem.GetByID(context.Background(), victim.ID); err != nil {
		t.Errorf("another account's memory was deleted: %v", err)
	}
	if rec := e.as(b, "DELETE", "/api/me/memories/"+victim.ID, nil); rec.Code != http.StatusNoContent {
		t.Errorf("deleting one's own memory = %d, want 204", rec.Code)
	}
}

func TestDeprecateKeepsTheRecord(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	m, err := e.mem.Store(context.Background(), memory.Memory{
		Scope: identity.TeamScope(tm.ID), Kind: memory.KindFact, Content: "superseded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.as(owner, "POST", "/api/teams/"+tm.ID+"/memories/"+m.ID+"/deprecate", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("deprecate = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	got, err := e.mem.GetByID(context.Background(), m.ID)
	if err != nil {
		t.Fatalf("the record should survive deprecation: %v", err)
	}
	if !got.Deprecated {
		t.Error("memory is not marked deprecated")
	}
}

// ---- self service ----

func TestSelfServiceRoutesNeedNoRole(t *testing.T) {
	e := newEnv(t)
	ordinary := e.user(identity.PlatformRoleUser)

	for _, path := range []string{"/api/me/usage", "/api/me/memories", "/api/me/tokens", "/api/teams"} {
		if rec := e.as(ordinary, "GET", path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s as an ordinary account = %d (%s), want 200", path, rec.Code, rec.Body.String())
		}
	}
}

func TestUpdateDisplayName(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	rec := e.as(u, "PATCH", "/api/me", map[string]string{"display_name": "New Name"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["user"].(map[string]any)["display_name"]; got != "New Name" {
		t.Errorf("display_name = %v, want New Name", got)
	}

	if rec := e.as(u, "PATCH", "/api/me", map[string]string{"display_name": "   "}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank display name = %d, want 400", rec.Code)
	}
}

func TestChangePasswordChecksCurrent(t *testing.T) {
	e := newEnv(t)
	store := e.store
	u, err := store.CreateUser(context.Background(), "chg-"+randSuffix()+"@test.dev", bcryptHash(t, "old-password"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })

	rec := e.as(u, "POST", "/api/me/password", map[string]string{
		"current_password": "wrong", "new_password": "new-password",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("wrong current password = %d (%s), want 403", rec.Code, rec.Body.String())
	}

	rec = e.as(u, "POST", "/api/me/password", map[string]string{
		"current_password": "old-password", "new_password": "short",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("too-short new password = %d, want 400", rec.Code)
	}

	rec = e.as(u, "POST", "/api/me/password", map[string]string{
		"current_password": "old-password", "new_password": "new-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid change = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	// Every session was cut, so the client must be told to sign in again
	// rather than discovering it via a surprise 401.
	if decodeBody(t, rec)["reauthenticate"] != true {
		t.Error("response does not tell the client to re-authenticate")
	}
}

func TestCreateTeamMakesCallerOwner(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	rec := e.as(u, "POST", "/api/teams", map[string]string{"name": "mine-" + randSuffix()})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create team = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)["team"].(map[string]any)
	teamID := created["id"].(string)
	t.Cleanup(func() { e.store.DeleteTeam(context.Background(), teamID) })

	if created["role"] != "owner" {
		t.Errorf("creator role = %v, want owner", created["role"])
	}
	role, member, err := e.store.RoleInTeam(context.Background(), teamID, u.ID)
	if err != nil || !member || role != identity.RoleOwner {
		t.Errorf("membership = %q, %v, %v; want owner", role, member, err)
	}
}

func TestCreateTeamRequiresName(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)
	if rec := e.as(u, "POST", "/api/teams", map[string]string{"name": "  "}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank team name = %d, want 400", rec.Code)
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	req := httptest.NewRequest("POST", "/api/teams", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	e.actor = u
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", rec.Code)
	}
}

func bcryptHash(t *testing.T, pw string) string {
	t.Helper()
	// Mirrors what Signup stores, so ChangePassword can verify it.
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return string(h)
}

// ---- malformed identifiers ----

// Ids come straight from URL path segments, so a typo or a probe reaches the
// database as a non-uuid. That must read as "not found", not as a server fault:
// a 500 is both wrong and noisy enough to page someone over a bad link.
func TestMalformedIdentifiersAreNotFoundNotServerErrors(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	owner := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)

	cases := []struct {
		name         string
		actor        identity.User
		method, path string
		body         any
		want         int
	}{
		{"read team", owner, "GET", "/api/teams/not-a-uuid", nil, http.StatusNotFound},
		{"list members", owner, "GET", "/api/teams/not-a-uuid/members", nil, http.StatusNotFound},
		{"rename team", owner, "PATCH", "/api/teams/not-a-uuid", map[string]string{"name": "x"}, http.StatusNotFound},
		{"delete team", owner, "DELETE", "/api/teams/not-a-uuid", nil, http.StatusNotFound},
		{
			"remove member", owner, "DELETE",
			"/api/teams/" + tm.ID + "/members/not-a-uuid", nil, http.StatusNotFound,
		},
		{
			"change member role", owner, "PATCH",
			"/api/teams/" + tm.ID + "/members/not-a-uuid", map[string]string{"role": "admin"}, http.StatusNotFound,
		},
		{"delete own memory", owner, "DELETE", "/api/me/memories/not-a-uuid", nil, http.StatusNotFound},
		{
			"delete team memory", owner, "DELETE",
			"/api/teams/" + tm.ID + "/memories/not-a-uuid", nil, http.StatusNotFound,
		},
		{"revoke session", owner, "DELETE", "/api/me/tokens/not-a-uuid", nil, http.StatusNotFound},
		{"patch account", admin, "PATCH", "/api/admin/users/not-a-uuid", map[string]string{"display_name": "x"}, http.StatusNotFound},
		{"delete account", admin, "DELETE", "/api/admin/users/not-a-uuid", nil, http.StatusNotFound},
		{"reset password", admin, "POST", "/api/admin/users/not-a-uuid/password", map[string]string{"password": "long-enough"}, http.StatusNotFound},
		{"delete memory", admin, "DELETE", "/api/admin/memories/not-a-uuid", nil, http.StatusNotFound},
	}

	for _, c := range cases {
		rec := e.as(c.actor, c.method, c.path, c.body)
		if rec.Code != c.want {
			t.Errorf("%s with a malformed id = %d (%s), want %d",
				c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// Reads that aggregate or list over a malformed owner id report an empty
// result rather than failing — the id names nothing, which is a truthful zero.
func TestMalformedIdentifiersOnReadsReturnEmpty(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	cases := []struct{ name, path string }{
		{"team usage", "/api/teams/not-a-uuid/usage"},
		{"team memories", "/api/teams/not-a-uuid/memories"},
		{"admin memories by user", "/api/admin/memories?scope=user&user_id=not-a-uuid"},
		{"admin memories by team", "/api/admin/memories?scope=team&team_id=not-a-uuid"},
	}
	for _, c := range cases {
		rec := e.as(admin, "GET", c.path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with a malformed id = %d (%s), want 200 with an empty result",
				c.name, rec.Code, rec.Body.String())
		}
	}
}
