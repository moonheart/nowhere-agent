package agentdefapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
)

// Like the skill console tests, this is mostly an authorization matrix plus
// the document-validation paths. It runs against the real development Postgres
// because the guards consult membership, with the definition store PG-backed.
// The test database IS the dev database, so every row uses a unique random
// name and cleanup deletes only what a test created, by ID — never an
// unscoped DELETE/UPDATE.

func testDSN() string {
	if v := os.Getenv("AGENTDEFAPI_PG_TEST_DSN"); v != "" {
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
	t     *testing.T
	db    *sql.DB
	store *identity.Store
	svc   *identity.Service
	defs  *agentdef.PGStore
	mux   *http.ServeMux

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

	e := &env{t: t, db: db, store: identity.NewStore(db), defs: agentdef.NewPGStore(db)}
	e.svc = identity.NewService(e.store)

	h := NewHandler(e.svc, e.defs, func(context.Context, []identity.ScopeRef) bool { return false })
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
	email := "agd-" + randSuffix() + "@test.dev"
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

// cleanupDef removes exactly the definition a test created, by ID (cascades to
// its versions).
func (e *env) cleanupDef(id string) {
	e.t.Cleanup(func() { e.db.Exec(`DELETE FROM agent_defs WHERE id = $1`, id) })
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
		t.Fatalf("decode %d body: %v (%s)", rec.Code, err, rec.Body.String())
	}
	return out
}

func doc(name, body string) map[string]any {
	return map[string]any{"document": fmt.Sprintf("---\nname: %s\ndescription: test %s\nskills: lint\n---\n%s\n", name, name, body)}
}

// TestSelfTierRoundTrip: create → list → get → update → delete at user scope,
// each operation confined to the caller's own definitions.
func TestSelfTierRoundTrip(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)
	name := "self-" + randSuffix()

	rec := e.as(u, "POST", "/api/me/agentdefs", doc(name, "You are helpful."))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	def := created["def"].(map[string]any)
	e.cleanupDef(def["id"].(string))
	if def["name"] != name || def["scope"] != "user" || def["user_id"] != u.ID {
		t.Fatalf("created def: %v", def)
	}
	// skills declared, runner unavailable (env stub returns false) → warning.
	if warns, _ := created["warnings"].([]any); len(warns) != 1 {
		t.Fatalf("expected a skills-runner warning, got %v", created["warnings"])
	}

	rec = e.as(u, "GET", "/api/me/agentdefs", nil)
	if rec.Code != http.StatusOK || len(decodeBody(t, rec)["defs"].([]any)) != 1 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	rec = e.as(u, "GET", "/api/me/agentdefs/"+name, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}

	rec = e.as(u, "PUT", "/api/me/agentdefs/"+name, doc(name, "Updated body."))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["def"].(map[string]any)["current_version"]; got != float64(2) {
		t.Fatalf("update must bump the version, got %v", got)
	}

	rec = e.as(u, "DELETE", "/api/me/agentdefs/"+name, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.as(u, "GET", "/api/me/agentdefs/"+name, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted def must 404, got %d", rec.Code)
	}
}

// TestSelfTierIsolation: another account cannot see, update, or delete my
// definition; cross-account access reads as not-found.
func TestSelfTierIsolation(t *testing.T) {
	e := newEnv(t)
	owner, other := e.user(identity.PlatformRoleUser), e.user(identity.PlatformRoleUser)
	name := "self-" + randSuffix()

	rec := e.as(owner, "POST", "/api/me/agentdefs", doc(name, "mine"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	e.cleanupDef(decodeBody(t, rec)["def"].(map[string]any)["id"].(string))

	if rec := e.as(other, "GET", "/api/me/agentdefs/"+name, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other account GET must 404, got %d", rec.Code)
	}
	if rec := e.as(other, "PUT", "/api/me/agentdefs/"+name, doc(name, "hijack")); rec.Code != http.StatusNotFound {
		t.Fatalf("other account PUT must 404, got %d", rec.Code)
	}
	if rec := e.as(other, "DELETE", "/api/me/agentdefs/"+name, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other account DELETE must 404, got %d", rec.Code)
	}
	rec = e.as(other, "GET", "/api/me/agentdefs", nil)
	if n := len(decodeBody(t, rec)["defs"].([]any)); n != 0 {
		t.Fatalf("other account list must be empty, got %d", n)
	}
}

// TestTeamTierAuthorization: members read, team admins write, non-members get
// 404, platform admins pass without membership.
func TestTeamTierAuthorization(t *testing.T) {
	e := newEnv(t)
	owner := e.user(identity.PlatformRoleUser)
	member := e.user(identity.PlatformRoleUser)
	outsider := e.user(identity.PlatformRoleUser)
	admin := e.user(identity.PlatformRoleAdmin)
	tm := e.team(owner)
	e.join(tm, member, identity.RoleMember)
	name := "team-" + randSuffix()
	base := fmt.Sprintf("/api/teams/%s/agentdefs", tm.ID)

	// Non-member: 404 (no existence leak) on both read and write.
	if rec := e.as(outsider, "GET", base, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider list must 404, got %d", rec.Code)
	}
	if rec := e.as(outsider, "POST", base, doc(name, "x")); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider create must 404, got %d", rec.Code)
	}

	// Member below write role: reads, cannot write.
	if rec := e.as(member, "GET", base, nil); rec.Code != http.StatusOK {
		t.Fatalf("member list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.as(member, "POST", base, doc(name, "x")); rec.Code != http.StatusForbidden {
		t.Fatalf("member create must 403, got %d", rec.Code)
	}

	// Team admin (owner) writes.
	rec := e.as(owner, "POST", base, doc(name, "team def"))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner create: %d %s", rec.Code, rec.Body.String())
	}
	def := decodeBody(t, rec)["def"].(map[string]any)
	e.cleanupDef(def["id"].(string))
	if def["scope"] != "team" || def["team_id"] != tm.ID {
		t.Fatalf("team def: %v", def)
	}
	if rec := e.as(member, "GET", base+"/"+name, nil); rec.Code != http.StatusOK {
		t.Fatalf("member read: %d %s", rec.Code, rec.Body.String())
	}

	// Platform admin without membership passes.
	if rec := e.as(admin, "GET", base, nil); rec.Code != http.StatusOK {
		t.Fatalf("platform admin team list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.as(admin, "DELETE", base+"/"+name, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("platform admin team delete: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPlatformTierAdminOnly: system scope is admin-only; a platform admin
// round-trips a system definition.
func TestPlatformTierAdminOnly(t *testing.T) {
	e := newEnv(t)
	plain := e.user(identity.PlatformRoleUser)
	admin := e.user(identity.PlatformRoleAdmin)
	name := "sys-" + randSuffix()

	if rec := e.as(plain, "GET", "/api/admin/agentdefs", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin list must 403, got %d", rec.Code)
	}
	if rec := e.as(plain, "POST", "/api/admin/agentdefs", doc(name, "x")); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin create must 403, got %d", rec.Code)
	}

	rec := e.as(admin, "POST", "/api/admin/agentdefs", doc(name, "system def"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin create: %d %s", rec.Code, rec.Body.String())
	}
	def := decodeBody(t, rec)["def"].(map[string]any)
	e.cleanupDef(def["id"].(string))
	if def["scope"] != "system" {
		t.Fatalf("system def: %v", def)
	}
	if rec := e.as(admin, "DELETE", "/api/admin/agentdefs/"+name, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete: %d %s", rec.Code, rec.Body.String())
	}
}

// TestInvalidDocumentRejected: unparseable frontmatter, missing name, and
// empty body all fail validation with 400 and store nothing.
func TestInvalidDocumentRejected(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	for _, body := range []map[string]any{
		{"document": "no frontmatter"},
		{"document": "---\ndescription: no name\n---\nbody\n"},
		{"document": "---\nname: x\ndescription: y\n---\n"},
		{"document": ""},
	} {
		if rec := e.as(u, "POST", "/api/me/agentdefs", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid doc must 400, got %d for %v", rec.Code, body)
		}
	}
	rec := e.as(u, "GET", "/api/me/agentdefs", nil)
	if n := len(decodeBody(t, rec)["defs"].([]any)); n != 0 {
		t.Fatalf("nothing must be stored, got %d defs", n)
	}
}

// TestBuiltinNotWritableThroughAPI: the built-in general-purpose definition is
// not in the store, so acting on it reads as not-found; overriding it means
// POSTing a same-named scoped definition, which then resolves.
func TestBuiltinNotWritableThroughAPI(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	if rec := e.as(u, "PUT", "/api/me/agentdefs/general-purpose", doc("general-purpose", "hijack")); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT on built-in-only name must 404 (not an upsert), got %d", rec.Code)
	}
	if rec := e.as(u, "DELETE", "/api/me/agentdefs/general-purpose", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE on built-in-only name must 404, got %d", rec.Code)
	}

	rec := e.as(u, "POST", "/api/me/agentdefs", doc("general-purpose", "my override"))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST same-named scoped def: %d %s", rec.Code, rec.Body.String())
	}
	def := decodeBody(t, rec)["def"].(map[string]any)
	e.cleanupDef(def["id"].(string))
	if def["name"] != "general-purpose" || def["scope"] != "user" {
		t.Fatalf("override def: %v", def)
	}
}
