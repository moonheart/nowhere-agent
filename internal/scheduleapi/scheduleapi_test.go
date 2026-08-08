package scheduleapi

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

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/schedule"
)

// These tests run against the real development Postgres because the handlers
// resolve the caller's tasks through the store. The test database IS the dev
// database, so every row uses a unique random owner and cleanup deletes only
// what a test created, by ID — never an unscoped DELETE/UPDATE.

func testDSN() string {
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
	store *schedule.PGStore
	mux   *http.ServeMux
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

	e := &env{t: t, db: db, store: schedule.NewPGStore(db)}
	h := NewHandler(e.store)
	e.mux = http.NewServeMux()
	h.RegisterAuthed(e.mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), e.actor)))
		})
	})
	return e
}

func (e *env) user() identity.User {
	e.t.Helper()
	var u identity.User
	err := e.db.QueryRow(`
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, 'x', $2) RETURNING id, email, display_name`,
		"sch-"+randSuffix()+"@test.dev", "u-"+randSuffix()).
		Scan(&u.ID, &u.Email, &u.DisplayName)
	if err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	e.t.Cleanup(func() { e.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID) })
	return u
}

func (e *env) cleanupTask(id string) {
	e.t.Cleanup(func() { e.db.Exec(`DELETE FROM scheduled_task WHERE id = $1`, id) })
}

func (e *env) as(u identity.User, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	e.actor = u
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
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

func validPayload() map[string]any {
	return map[string]any{
		"prompt":         "daily summary",
		"cron":           "0 9 * * *",
		"timezone":       "UTC",
		"tool_whitelist": []string{"read_file"},
	}
}

func TestCreateAndGet(t *testing.T) {
	e := newEnv(t)
	owner := e.user()

	rec := e.as(owner, "POST", "/api/me/scheduled-tasks", validPayload())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body)
	}
	task := decodeBody(t, rec)["task"].(map[string]any)
	id := task["id"].(string)
	e.cleanupTask(id)
	if task["cron"] != "0 9 * * *" || task["next_run_at"] == nil {
		t.Fatalf("created task malformed: %v", task)
	}

	rec = e.as(owner, "GET", "/api/me/scheduled-tasks/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d", rec.Code)
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	owner := e.user()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"no prompt source", map[string]any{"cron": "0 9 * * *"}},
		{"bad cron", map[string]any{"prompt": "x", "cron": "nope"}},
		{"bad timezone", map[string]any{"prompt": "x", "cron": "0 9 * * *", "timezone": "Mars/Olympus"}},
		{"bad multitask", map[string]any{"prompt": "x", "cron": "0 9 * * *", "multitask_strategy": "explode"}},
	}
	for _, c := range cases {
		rec := e.as(owner, "POST", "/api/me/scheduled-tasks", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d (%s)", c.name, rec.Code, rec.Body)
		}
	}
}

func TestListConfinedToOwner(t *testing.T) {
	e := newEnv(t)
	alice := e.user()
	bob := e.user()

	rec := e.as(alice, "POST", "/api/me/scheduled-tasks", validPayload())
	id := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)
	e.cleanupTask(id)

	// Alice sees her task.
	rec = e.as(alice, "GET", "/api/me/scheduled-tasks", nil)
	tasks := decodeBody(t, rec)["tasks"].([]any)
	found := false
	for _, tk := range tasks {
		if tk.(map[string]any)["id"] == id {
			found = true
		}
	}
	if !found {
		t.Error("owner should see own task in list")
	}

	// Bob does not.
	rec = e.as(bob, "GET", "/api/me/scheduled-tasks", nil)
	for _, tk := range decodeBody(t, rec)["tasks"].([]any) {
		if tk.(map[string]any)["id"] == id {
			t.Error("other user's task leaked into list")
		}
	}
}

func TestCrossOwnerDenied(t *testing.T) {
	e := newEnv(t)
	alice := e.user()
	bob := e.user()

	rec := e.as(alice, "POST", "/api/me/scheduled-tasks", validPayload())
	id := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)
	e.cleanupTask(id)

	// Bob cannot read, update, delete, enable, or list sessions of Alice's task —
	// each reads as not-found, never forbidden (no id probing).
	for _, op := range []struct{ method, path string }{
		{"GET", "/api/me/scheduled-tasks/" + id},
		{"PUT", "/api/me/scheduled-tasks/" + id},
		{"DELETE", "/api/me/scheduled-tasks/" + id},
		{"POST", "/api/me/scheduled-tasks/" + id + "/enable"},
		{"GET", "/api/me/scheduled-tasks/" + id + "/sessions"},
	} {
		rec := e.as(bob, op.method, op.path, validPayload())
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s by outsider: expected 404, got %d", op.method, op.path, rec.Code)
		}
	}
}

func TestUpdateAndEnable(t *testing.T) {
	e := newEnv(t)
	owner := e.user()

	rec := e.as(owner, "POST", "/api/me/scheduled-tasks", validPayload())
	task := decodeBody(t, rec)["task"].(map[string]any)
	id := task["id"].(string)
	e.cleanupTask(id)

	// Update the schedule; next_run_at recomputes.
	body := validPayload()
	body["cron"] = "30 14 * * *"
	rec = e.as(owner, "PUT", "/api/me/scheduled-tasks/"+id, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d body %s", rec.Code, rec.Body)
	}
	updated := decodeBody(t, rec)["task"].(map[string]any)
	if updated["cron"] != "30 14 * * *" {
		t.Fatalf("update did not apply: %v", updated)
	}

	// Disable then re-enable; an update must not have flipped the gate.
	rec = e.as(owner, "POST", "/api/me/scheduled-tasks/"+id+"/disable", nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["task"].(map[string]any)["enabled"] != false {
		t.Fatalf("disable: status %d body %s", rec.Code, rec.Body)
	}
	rec = e.as(owner, "POST", "/api/me/scheduled-tasks/"+id+"/enable", nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["task"].(map[string]any)["enabled"] != true {
		t.Fatalf("enable: status %d body %s", rec.Code, rec.Body)
	}
}

func TestSessionsEndpoint(t *testing.T) {
	e := newEnv(t)
	owner := e.user()

	rec := e.as(owner, "POST", "/api/me/scheduled-tasks", validPayload())
	id := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)
	e.cleanupTask(id)

	// Tag a session to the task directly (mirroring what the trigger writes).
	var sessID string
	e.db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'s',$2,'scheduled') RETURNING id`, owner.ID, id).Scan(&sessID)
	e.t.Cleanup(func() { e.db.Exec(`DELETE FROM sessions WHERE id = $1`, sessID) })

	rec = e.as(owner, "GET", "/api/me/scheduled-tasks/"+id+"/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sessions: status %d", rec.Code)
	}
	sessions := decodeBody(t, rec)["sessions"].([]any)
	if len(sessions) != 1 || sessions[0] != sessID {
		t.Fatalf("expected [%s], got %v", sessID, sessions)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	owner := e.user()

	rec := e.as(owner, "POST", "/api/me/scheduled-tasks", validPayload())
	id := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)

	rec = e.as(owner, "DELETE", "/api/me/scheduled-tasks/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", rec.Code)
	}
	rec = e.as(owner, "GET", "/api/me/scheduled-tasks/"+id, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rec.Code)
	}
}

func TestMalformedID(t *testing.T) {
	e := newEnv(t)
	owner := e.user()
	rec := e.as(owner, "GET", "/api/me/scheduled-tasks/not-a-uuid", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id should be 404, got %d", rec.Code)
	}
}

func TestClearSessions(t *testing.T) {
	e := newEnv(t)
	owner := e.user()

	rec := e.as(owner, "POST", "/api/me/scheduled-tasks", validPayload())
	id := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)
	e.cleanupTask(id)

	// Two sessions on the task.
	var s1, s2 string
	e.db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'a',$2,'scheduled') RETURNING id`, owner.ID, id).Scan(&s1)
	e.db.QueryRow(`INSERT INTO sessions (user_id, title, task_id, source) VALUES ($1,'b',$2,'scheduled') RETURNING id`, owner.ID, id).Scan(&s2)
	e.t.Cleanup(func() { e.db.Exec(`DELETE FROM sessions WHERE id IN ($1,$2)`, s1, s2) })

	rec = e.as(owner, "POST", "/api/me/scheduled-tasks/"+id+"/sessions/clear", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status %d body %s", rec.Code, rec.Body)
	}
	if cleared := decodeBody(t, rec)["cleared"].(float64); cleared != 2 {
		t.Fatalf("expected cleared=2, got %v", cleared)
	}

	// The list is now empty; the rows remain (soft-delete).
	rec = e.as(owner, "GET", "/api/me/scheduled-tasks/"+id+"/sessions", nil)
	if n := len(decodeBody(t, rec)["sessions"].([]any)); n != 0 {
		t.Fatalf("expected sessions empty after clear, got %d", n)
	}
	var status string
	if err := e.db.QueryRow(`SELECT status FROM sessions WHERE id = $1`, s1).Scan(&status); err != nil {
		t.Fatalf("read cleared row: %v", err)
	}
	if status != "ended" {
		t.Fatalf("expected status=ended, got %q", status)
	}
}

func TestClearSessionsCrossOwnerDenied(t *testing.T) {
	e := newEnv(t)
	owner := e.user()
	other := e.user()

	rec := e.as(owner, "POST", "/api/me/scheduled-tasks", validPayload())
	id := decodeBody(t, rec)["task"].(map[string]any)["id"].(string)
	e.cleanupTask(id)

	// Another user's clear attempt reads as not-found (no probing).
	rec = e.as(other, "POST", "/api/me/scheduled-tasks/"+id+"/sessions/clear", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner clear: expected 404, got %d", rec.Code)
	}
}
