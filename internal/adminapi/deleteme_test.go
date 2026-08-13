package adminapi

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"nowhere-agent/internal/identity"
)

// TestDeleteMe is the PIPL §47 erasure end-to-end: the account owner removes
// their own account and everything cascading from it. It pins the regression
// where DELETE /api/me hit DeleteAccount's admin-only self-target guard and
// answered 409 forever.
func TestDeleteMe(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	// Give the account data that must cascade away: a session + a token.
	sessID := seedSession(t, e.db, u.ID)
	token := seedToken(t, e.db, u.ID)

	rec := e.as(u, "DELETE", "/api/me", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete me: %d (%s), want 204", rec.Code, rec.Body)
	}

	// The account row is gone (and with it the cascaded session/token).
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM users WHERE id = $1`, u.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user row remains (n=%d err=%v)", n, err)
	}
	if err := e.db.QueryRow(`SELECT count(*) FROM sessions WHERE id = $1`, sessID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("session row remains (n=%d err=%v)", n, err)
	}
	if err := e.db.QueryRow(`SELECT count(*) FROM auth_tokens WHERE id = $1`, token).Scan(&n); err != nil || n != 0 {
		t.Fatalf("token row remains (n=%d err=%v)", n, err)
	}
	// Deleting again is a 404 (the account no longer exists to own anything).
	rec = e.as(u, "DELETE", "/api/me", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", rec.Code)
	}
}

// seedSession creates a session row for userID and returns its id.
func seedSession(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`INSERT INTO sessions (user_id, title) VALUES ($1, 't') RETURNING id`, userID).Scan(&id); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, id) })
	return id
}

// seedToken creates an auth token row for userID and returns its id.
func seedToken(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`INSERT INTO auth_tokens (user_id, token_hash, expires_at)
		 VALUES ($1, 'x', now() + interval '1 day') RETURNING id`, userID).Scan(&id); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM auth_tokens WHERE id = $1`, id) })
	return id
}

// TestDeleteMeServiceGuard pins the service split: DeleteAccount refuses a
// self-target (admin semantics), DeleteSelf allows it (the erasure right).
func TestDeleteMeServiceGuard(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	if err := e.svc.DeleteAccount(context.Background(), u.ID, u.ID); err != identity.ErrSelfTarget {
		t.Fatalf("DeleteAccount(self) = %v, want ErrSelfTarget", err)
	}
	if err := e.svc.DeleteSelf(context.Background(), u.ID); err != nil {
		t.Fatalf("DeleteSelf(self) = %v, want nil", err)
	}
}

// TestDeleteMePurgesImages pins the regression where DELETE /api/me wiped the
// account but left its workspace images behind: the session's image dir and
// the user's __uploads__ scope must be removed just like the admin delete
// path does (the account row and its cascades die, so nothing else will ever
// clean those dirs).
func TestDeleteMePurgesImages(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)
	sess := e.sessionFor(u)

	// Seed the two workspace locations the delete must reclaim: a session
	// image dir and the user's upload scope.
	sessDir := filepath.Join(e.images.Root(), sess.ID)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "pic.webp"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write session image: %v", err)
	}
	uploadDir := filepath.Join(e.images.Root(), "__uploads__", u.ID)
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatalf("mkdir upload dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "blob.webp"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write upload blob: %v", err)
	}

	rec := e.as(u, "DELETE", "/api/me", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete me: %d (%s), want 204", rec.Code, rec.Body)
	}
	for path, what := range map[string]string{
		filepath.Join(sessDir, "pic.webp"):    "session image",
		filepath.Join(uploadDir, "blob.webp"): "upload blob",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the delete (stat err=%v)", what, err)
		}
	}
}
