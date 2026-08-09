package skillapi

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
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/skill"
)

// Like the admin console tests, this is mostly an authorization matrix: for each
// tier, which of {outsider, member, team admin, platform admin} gets through. It
// runs against the real development Postgres because the guards consult
// membership, with the skill store also PG-backed. The test database IS the dev
// database, so every row uses a unique random name and cleanup deletes only what
// a test created, by ID — never an unscoped DELETE/UPDATE.

func testDSN() string {
	if v := os.Getenv("SKILLAPI_PG_TEST_DSN"); v != "" {
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

type env struct {
	t      *testing.T
	db     *sql.DB
	store  *identity.Store
	svc    *identity.Service
	skills *skill.PGStore
	mux    *http.ServeMux

	// actor is who the fake auth middleware presents as the caller.
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

	e := &env{t: t, db: db, store: identity.NewStore(db), skills: skill.NewPGStore(db)}
	e.svc = identity.NewService(e.store)

	h := NewHandler(e.svc, e.skills)
	e.mux = http.NewServeMux()
	authed := httpx.NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), e.actor)))
		})
	})
	h.RegisterAuthed(authed)
	authed.Mount(e.mux, "/api/")
	return e
}

func (e *env) user(role identity.PlatformRole) identity.User {
	e.t.Helper()
	var u identity.User
	email := "skl-" + randSuffix() + "@test.dev"
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

// cleanupSkill removes exactly the skill a test created, by ID (cascades to its
// versions).
func (e *env) cleanupSkill(id string) {
	e.t.Cleanup(func() { e.db.Exec(`DELETE FROM skills WHERE id = $1`, id) })
}

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

// skillPayload is a minimal valid create/update body.
func skillPayload(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "d",
		"body":        "body",
		"resources":   map[string]string{},
		"scripts":     map[string]string{},
	}
}

// createdSkillID extracts the skill id from a create response and registers
// cleanup.
func (e *env) createdSkillID(rec *httptest.ResponseRecorder) string {
	e.t.Helper()
	id := decodeBody(e.t, rec)["skill"].(map[string]any)["id"].(string)
	e.cleanupSkill(id)
	return id
}

// ---- self tier ----

func TestSelfSkillLifecycle(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)
	name := "self-" + randSuffix()

	// Create → 201, version 1.
	rec := e.as(u, "POST", "/api/me/skills", skillPayload(name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	id := e.createdSkillID(rec)
	if v := decodeBody(t, rec)["skill"].(map[string]any)["current_version"]; v != float64(1) {
		t.Errorf("new skill version = %v, want 1", v)
	}

	// Update → version 2.
	rec = e.as(u, "PUT", "/api/me/skills/"+id, skillPayload(name))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if v := decodeBody(t, rec)["skill"].(map[string]any)["current_version"]; v != float64(2) {
		t.Errorf("updated skill version = %v, want 2", v)
	}

	// Versions endpoint lists both revisions.
	rec = e.as(u, "GET", "/api/me/skills/"+id+"/versions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("versions = %d", rec.Code)
	}
	if n := len(decodeBody(t, rec)["versions"].([]any)); n != 2 {
		t.Errorf("versions = %d, want 2", n)
	}

	// Rollback to v1 → version 3.
	rec = e.as(u, "POST", "/api/me/skills/"+id+"/rollback/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if v := decodeBody(t, rec)["skill"].(map[string]any)["current_version"]; v != float64(3) {
		t.Errorf("rollback version = %v, want 3", v)
	}

	// Delete → 204, then get is 404.
	if rec := e.as(u, "DELETE", "/api/me/skills/"+id, nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", rec.Code)
	}
	if rec := e.as(u, "GET", "/api/me/skills/"+id, nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", rec.Code)
	}
}

// One account must not be able to read or change another's skill through the
// self route: a foreign id reads as not-found.
func TestSelfSkillIsScopedToCaller(t *testing.T) {
	e := newEnv(t)
	a := e.user(identity.PlatformRoleUser)
	b := e.user(identity.PlatformRoleUser)

	rec := e.as(b, "POST", "/api/me/skills", skillPayload("b-"+randSuffix()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create as b = %d", rec.Code)
	}
	id := e.createdSkillID(rec)

	for _, method := range []string{"GET", "PUT", "DELETE"} {
		var body any
		if method == "PUT" {
			body = skillPayload("x")
		}
		if rec := e.as(a, method, "/api/me/skills/"+id, body); rec.Code != http.StatusNotFound {
			t.Errorf("%s another account's skill = %d, want 404", method, rec.Code)
		}
	}
}

func TestSelfSkillValidation(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	if rec := e.as(u, "POST", "/api/me/skills", map[string]any{"name": "  "}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank name = %d, want 400", rec.Code)
	}
	bad := skillPayload("v-" + randSuffix())
	bad["scripts"] = map[string]string{"../escape.py": "x"}
	if rec := e.as(u, "POST", "/api/me/skills", bad); rec.Code != http.StatusBadRequest {
		t.Errorf("parent-traversal script path = %d, want 400", rec.Code)
	}
	bad["scripts"] = map[string]string{"/abs.py": "x"}
	if rec := e.as(u, "POST", "/api/me/skills", bad); rec.Code != http.StatusBadRequest {
		t.Errorf("absolute script path = %d, want 400", rec.Code)
	}
}

// ---- team tier ----

func TestTeamSkillAuthorizationMatrix(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	teamAdmin := e.user(identity.PlatformRoleUser)
	member := e.user(identity.PlatformRoleUser)
	outsider := e.user(identity.PlatformRoleUser)
	platformAdmin := e.user(identity.PlatformRoleAdmin)

	tm := e.team(owner)
	e.join(tm, teamAdmin, identity.RoleAdmin)
	e.join(tm, member, identity.RoleMember)

	// A team admin creates the skill.
	rec := e.as(teamAdmin, "POST", "/api/teams/"+tm.ID+"/skills", skillPayload("t-"+randSuffix()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("team admin create = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	id := e.createdSkillID(rec)

	// A plain member can read but not write.
	if rec := e.as(member, "GET", "/api/teams/"+tm.ID+"/skills", nil); rec.Code != http.StatusOK {
		t.Errorf("member list = %d, want 200", rec.Code)
	}
	if rec := e.as(member, "GET", "/api/teams/"+tm.ID+"/skills/"+id, nil); rec.Code != http.StatusOK {
		t.Errorf("member read = %d, want 200", rec.Code)
	}
	if rec := e.as(member, "PUT", "/api/teams/"+tm.ID+"/skills/"+id, skillPayload("x")); rec.Code != http.StatusForbidden {
		t.Errorf("member write = %d, want 403", rec.Code)
	}
	if rec := e.as(member, "POST", "/api/teams/"+tm.ID+"/skills", skillPayload("y")); rec.Code != http.StatusForbidden {
		t.Errorf("member create = %d, want 403", rec.Code)
	}

	// An outsider gets 404 (anti-enumeration), on both read and write.
	if rec := e.as(outsider, "GET", "/api/teams/"+tm.ID+"/skills", nil); rec.Code != http.StatusNotFound {
		t.Errorf("outsider list = %d, want 404", rec.Code)
	}
	if rec := e.as(outsider, "POST", "/api/teams/"+tm.ID+"/skills", skillPayload("z")); rec.Code != http.StatusNotFound {
		t.Errorf("outsider create = %d, want 404", rec.Code)
	}

	// A platform admin passes team routes without a membership.
	if rec := e.as(platformAdmin, "GET", "/api/teams/"+tm.ID+"/skills", nil); rec.Code != http.StatusOK {
		t.Errorf("platform admin list = %d, want 200", rec.Code)
	}
}

// A team admin must not reach another team's skill through their own team's
// route: the scope check reads it as not-found.
func TestTeamSkillScopeEnforcement(t *testing.T) {
	e := newEnv(t)
	ownerA := e.user(identity.PlatformRoleUser)
	ownerB := e.user(identity.PlatformRoleUser)
	teamA := e.team(ownerA)
	teamB := e.team(ownerB)

	rec := e.as(ownerB, "POST", "/api/teams/"+teamB.ID+"/skills", skillPayload("b-"+randSuffix()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create in team B = %d", rec.Code)
	}
	victim := e.createdSkillID(rec)

	if rec := e.as(ownerA, "GET", "/api/teams/"+teamA.ID+"/skills/"+victim, nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-team read = %d, want 404", rec.Code)
	}
	if rec := e.as(ownerA, "DELETE", "/api/teams/"+teamA.ID+"/skills/"+victim, nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-team delete = %d, want 404", rec.Code)
	}
}

// ---- platform tier ----

func TestPlatformSkillRequiresAdmin(t *testing.T) {
	e := newEnv(t)
	ordinary := e.user(identity.PlatformRoleUser)
	admin := e.user(identity.PlatformRoleAdmin)

	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/admin/skills"},
		{"POST", "/api/admin/skills"},
	} {
		var body any
		if rt.method == "POST" {
			body = skillPayload("x")
		}
		if rec := e.as(ordinary, rt.method, rt.path, body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as non-admin = %d, want 403", rt.method, rt.path, rec.Code)
		}
	}

	// The admin can create a system skill.
	rec := e.as(admin, "POST", "/api/admin/skills", skillPayload("sys-"+randSuffix()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create system skill = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	e.createdSkillID(rec) // register cleanup for the created system skill
	if scope := decodeBody(t, rec)["skill"].(map[string]any)["scope"]; scope != "system" {
		t.Errorf("platform skill scope = %v, want system", scope)
	}
}

// ---- enable / disable ----

// TestSelfSkillEnableDisable: a user disables and re-enables their own skill;
// the DTO reports the flag and the skill stays readable while disabled.
func TestSelfSkillEnableDisable(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	rec := e.as(u, "POST", "/api/me/skills", skillPayload("enab-"+randSuffix()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	id := e.createdSkillID(rec)
	if en := decodeBody(t, rec)["skill"].(map[string]any)["enabled"]; en != true {
		t.Fatalf("new skill enabled = %v, want true", en)
	}

	rec = e.as(u, "POST", "/api/me/skills/"+id+"/disable", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d (%s)", rec.Code, rec.Body.String())
	}
	if en := decodeBody(t, rec)["skill"].(map[string]any)["enabled"]; en != false {
		t.Errorf("after disable enabled = %v, want false", en)
	}

	// Still readable (and still reporting disabled) in the management surface.
	rec = e.as(u, "GET", "/api/me/skills/"+id, nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["skill"].(map[string]any)["enabled"] != false {
		t.Errorf("get while disabled = %d enabled=%v, want 200/false", rec.Code, decodeBody(t, rec)["skill"].(map[string]any)["enabled"])
	}

	rec = e.as(u, "POST", "/api/me/skills/"+id+"/enable", nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["skill"].(map[string]any)["enabled"] != true {
		t.Errorf("enable = %d enabled=%v, want 200/true", rec.Code, decodeBody(t, rec)["skill"].(map[string]any)["enabled"])
	}
}

// TestEnableDisableScopeEnforcement: a user cannot toggle another account's
// skill, and team toggles require team admin.
func TestEnableDisableScopeEnforcement(t *testing.T) {
	e := newEnv(t)
	a := e.user(identity.PlatformRoleUser)
	b := e.user(identity.PlatformRoleUser)

	rec := e.as(b, "POST", "/api/me/skills", skillPayload("x-"+randSuffix()))
	id := e.createdSkillID(rec)
	for _, op := range []string{"enable", "disable"} {
		if rec := e.as(a, "POST", "/api/me/skills/"+id+"/"+op, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s another account's skill = %d, want 404", op, rec.Code)
		}
	}

	// Team tier: a plain member cannot toggle; the team admin can.
	owner := e.user(identity.PlatformRoleUser)
	teamAdmin := e.user(identity.PlatformRoleUser)
	member := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	e.join(tm, teamAdmin, identity.RoleAdmin)
	e.join(tm, member, identity.RoleMember)
	rec = e.as(teamAdmin, "POST", "/api/teams/"+tm.ID+"/skills", skillPayload("t-"+randSuffix()))
	tid := e.createdSkillID(rec)
	if rec := e.as(member, "POST", "/api/teams/"+tm.ID+"/skills/"+tid+"/disable", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member disable team skill = %d, want 403", rec.Code)
	}
	if rec := e.as(teamAdmin, "POST", "/api/teams/"+tm.ID+"/skills/"+tid+"/disable", nil); rec.Code != http.StatusOK {
		t.Errorf("team admin disable = %d, want 200", rec.Code)
	}
}

// ---- move to team ----

// TestMoveMySkillToTeam: a team admin moves their own user skill into their
// team; the skill then resolves under the team scope and reports the new scope.
func TestMoveMySkillToTeam(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleUser)
	tm := e.team(admin) // owner is implicitly a member; make them admin below
	e.join(tm, admin, identity.RoleAdmin)

	rec := e.as(admin, "POST", "/api/me/skills", skillPayload("mv-"+randSuffix()))
	id := e.createdSkillID(rec)

	rec = e.as(admin, "POST", "/api/me/skills/"+id+"/move", map[string]any{"team_id": tm.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("move = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	sk := decodeBody(t, rec)["skill"].(map[string]any)
	if sk["scope"] != "team" || sk["team_id"] != tm.ID {
		t.Errorf("moved skill scope = %v team_id = %v, want team/%s", sk["scope"], sk["team_id"], tm.ID)
	}

	// It now appears in the team's list, not the user's.
	if rec := e.as(admin, "GET", "/api/me/skills/"+id, nil); rec.Code != http.StatusNotFound {
		t.Errorf("get moved skill via self route = %d, want 404", rec.Code)
	}
	if rec := e.as(admin, "GET", "/api/teams/"+tm.ID+"/skills/"+id, nil); rec.Code != http.StatusOK {
		t.Errorf("get moved skill via team route = %d, want 200", rec.Code)
	}
}

// TestMoveMySkillAuthorization: a plain member cannot move a skill into their
// team (403), an outsider cannot target the team at all (404), and another
// account's skill reads as not-found (404).
func TestMoveMySkillAuthorization(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	member := e.user(identity.PlatformRoleUser)
	outsider := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	e.join(tm, member, identity.RoleMember)

	// The member owns a skill but is only a member of the team -> 403.
	rec := e.as(member, "POST", "/api/me/skills", skillPayload("m-"+randSuffix()))
	mid := e.createdSkillID(rec)
	if rec := e.as(member, "POST", "/api/me/skills/"+mid+"/move", map[string]any{"team_id": tm.ID}); rec.Code != http.StatusForbidden {
		t.Errorf("member move into team = %d, want 403", rec.Code)
	}

	// The outsider owns a skill but is not in the team -> 404 (anti-enumeration).
	rec = e.as(outsider, "POST", "/api/me/skills", skillPayload("o-"+randSuffix()))
	oid := e.createdSkillID(rec)
	if rec := e.as(outsider, "POST", "/api/me/skills/"+oid+"/move", map[string]any{"team_id": tm.ID}); rec.Code != http.StatusNotFound {
		t.Errorf("outsider move into team = %d, want 404", rec.Code)
	}

	// The owner cannot move the member's skill (it is not theirs) -> 404.
	if rec := e.as(owner, "POST", "/api/me/skills/"+mid+"/move", map[string]any{"team_id": tm.ID}); rec.Code != http.StatusNotFound {
		t.Errorf("move another account's skill = %d, want 404", rec.Code)
	}
}

// TestMoveMySkillConflict: moving onto a team that already has a skill of the
// same name is a 409, and the source skill stays put.
func TestMoveMySkillConflict(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleUser)
	tm := e.team(admin)
	e.join(tm, admin, identity.RoleAdmin)
	name := "conf-" + randSuffix()

	// The team already owns a skill with this name.
	rec := e.as(admin, "POST", "/api/teams/"+tm.ID+"/skills", skillPayload(name))
	e.createdSkillID(rec)
	// The user owns a same-named user skill.
	rec = e.as(admin, "POST", "/api/me/skills", skillPayload(name))
	uid := e.createdSkillID(rec)

	if rec := e.as(admin, "POST", "/api/me/skills/"+uid+"/move", map[string]any{"team_id": tm.ID}); rec.Code != http.StatusConflict {
		t.Errorf("move onto existing = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	// The source skill is still a user skill.
	if rec := e.as(admin, "GET", "/api/me/skills/"+uid, nil); rec.Code != http.StatusOK {
		t.Errorf("source skill after conflict = %d, want still 200", rec.Code)
	}
}

// ---- store unavailable ----

func TestStoreUnavailableAnswers503(t *testing.T) {
	e := newEnv(t)
	h := NewHandler(e.svc, nil) // no store wired
	mux := http.NewServeMux()
	u := e.user(identity.PlatformRoleUser)
	authed := httpx.NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), u)))
		})
	})
	h.RegisterAuthed(authed)
	authed.Mount(mux, "/api/")
	req := httptest.NewRequest("GET", "/api/me/skills", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store = %d, want 503", rec.Code)
	}
}
