package identity

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/audit"
)

// TestLogoutFailureRecorded pins the logout failure path: when token
// revocation fails, the response stays 204 (logout is idempotent — the client
// has already discarded the token, and a 500 would not make it any safer)
// while the failure lands on the audit trail with outcome=failure.
func TestLogoutFailureRecorded(t *testing.T) {
	auditDB := pgTestDB(t)

	// The store's db is closed up front, so DeleteToken fails deterministically
	// (sql.ErrConnDone) without needing a Postgres failure.
	closed, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()

	h := NewHandler(NewService(NewStore(closed)))
	h.WithAudit(audit.NewLogger(auditDB, nil))

	user := User{ID: "u-" + randSuffix(), Email: "logout-fail-" + randSuffix() + "@example.com"}
	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	req = req.WithContext(NewContextWithUser(req.Context(), user))
	req.Header.Set("Authorization", "Bearer some-token")

	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204 (idempotent)", rec.Code)
	}

	// The failure must be on the trail: the newest auth.logout row for this
	// actor is the one just recorded.
	var id int64
	var outcome string
	err = auditDB.QueryRowContext(context.Background(), `
		SELECT id, outcome FROM audit_log
		WHERE actor_id = $1 AND action = 'auth.logout'
		ORDER BY id DESC LIMIT 1`, user.ID).Scan(&id, &outcome)
	if err != nil {
		t.Fatalf("no logout audit row for actor %s: %v", user.ID, err)
	}
	if outcome != "failure" {
		t.Errorf("logout audit outcome = %q, want failure", outcome)
	}
	t.Cleanup(func() {
		_, _ = auditDB.ExecContext(context.Background(), `DELETE FROM audit_log WHERE id = $1`, id)
	})
}
