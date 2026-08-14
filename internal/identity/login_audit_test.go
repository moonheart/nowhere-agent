package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/audit"
)

// TestDisabledLoginAuditCarriesEmail pins the identity clue of a disabled
// account's login attempt: the service returns a zero-value User (no actor),
// so the audit row's detail must carry the attempted email — the only
// identity clue a security review can follow — mirroring the
// invalid_credentials branch.
func TestDisabledLoginAuditCarriesEmail(t *testing.T) {
	db := pgTestDB(t)
	store := NewStore(db)
	svc := NewService(store)

	// Signup (bcrypt hashes the password) then disable the account; the user
	// row and its cascades are cleaned up by id.
	email := "audit-disabled-" + randSuffix() + "@example.com"
	u, err := svc.Signup(context.Background(), email, "password123", "audit-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID) })
	if err := store.SetUserDisabled(context.Background(), u.ID, true); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(svc).WithAudit(audit.NewLogger(db, nil))
	req := httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"email":"`+email+`","password":"password123"}`))
	req.Header.Set("Content-Type", "application/json")

	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("login status = %d, want 403 for a disabled account", rec.Code)
	}

	var id int64
	err = db.QueryRowContext(context.Background(), `
		SELECT id FROM audit_log
		WHERE action = 'auth.login'
		  AND detail->>'reason' = 'account_disabled'
		  AND detail->>'email' = $1
		ORDER BY id DESC LIMIT 1`, email).Scan(&id)
	if err != nil {
		t.Fatalf("no disabled-login audit row carrying email %s: %v", email, err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM audit_log WHERE id = $1`, id)
	})
}
