package adminapi

import (
	"context"
	"net/http"
	"testing"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/quota"
)

// quotaEnv wires a console with a real quota store over the dev Postgres, so
// the set/get/clear round trip is exercised end to end through HTTP. The owner
// ids are random per test and the budget rows are deleted on cleanup (scoped to
// exactly the rows created), per the test-DB convention.
func quotaEnv(t *testing.T) (*env, *quota.Store) {
	t.Helper()
	e := newEnv(t)
	qs := quota.NewStore(e.db)
	e.mux = http.NewServeMux()
	authed := httpx.NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), e.actor)))
		})
	})
	NewHandler(e.svc, nil, nil, e.mem).
		WithQuotas(qs).
		RegisterAuthed(authed)
	authed.Mount(e.mux, "/api/")
	return e, qs
}

// The quota routes are the management face of budget enforcement (P1-1). They
// are a platform-administration surface — a budget throttles someone else's
// spend — so the matrix matters more than the round-trip: an ordinary account
// must never read or write a cap.

func TestQuotaRoutesRequirePlatformAdmin(t *testing.T) {
	e := newEnv(t)
	ordinary := e.user(identity.PlatformRoleUser)
	owner := e.user(identity.PlatformRoleUser)
	e.team(owner) // owning a team confers no platform authority
	admin := e.user(identity.PlatformRoleAdmin)

	// newEnv wires no quota store (NewHandler leaves it nil), so an authorized
	// caller reaches the 503 guard; the point here is the 403 that precedes it.
	routes := []struct{ method, path string }{
		{"GET", "/api/admin/quotas?scope=user&owner_id=x"},
		{"PUT", "/api/admin/quotas"},
		{"DELETE", "/api/admin/quotas?scope=user&owner_id=x"},
	}
	for _, rt := range routes {
		for _, u := range []identity.User{ordinary, owner} {
			var body any
			if rt.method == "PUT" {
				body = map[string]any{"scope": "user", "owner_id": "x", "monthly_tokens": 1000}
			}
			if rec := e.as(u, rt.method, rt.path, body); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as non-admin = %d, want 403", rt.method, rt.path, rec.Code)
			}
		}
		// An admin passes the guard; with no store wired the route answers 503.
		var body any
		if rt.method == "PUT" {
			body = map[string]any{"scope": "user", "owner_id": "x", "monthly_tokens": 1000}
		}
		if rec := e.as(admin, rt.method, rt.path, body); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s as admin (no store) = %d (%s), want 503", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestPlatformUsageGroupsByModel(t *testing.T) {
	e := newEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	rec := e.as(admin, "GET", "/api/admin/usage?group_by=model", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group_by=model = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["group_by"]; got != "model" {
		t.Errorf("group_by = %v, want model", got)
	}
}

func TestQuotaSetGetClearRoundTrip(t *testing.T) {
	e, qs := quotaEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	owner := "qt-http-" + randSuffix()
	t.Cleanup(func() {
		_, _ = e.db.Exec(`DELETE FROM usage_budgets WHERE scope = 'user' AND owner_id = $1`, owner)
	})

	// Nothing set yet: 200 with budget null (the "no limit" state, not an error).
	rec := e.as(admin, "GET", "/api/admin/quotas?scope=user&owner_id="+owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get absent = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["budget"]; got != nil {
		t.Errorf("absent budget should decode as null, got %v", got)
	}

	// Set it.
	rec = e.as(admin, "PUT", "/api/admin/quotas", map[string]any{
		"scope": "user", "owner_id": owner, "monthly_tokens": 500000,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	b := decodeBody(t, rec)["budget"].(map[string]any)
	if b["monthly_tokens"].(float64) != 500000 || b["owner_id"] != owner {
		t.Errorf("put round trip mismatch: %v", b)
	}

	// Read it back through the store too, to confirm the row landed.
	stored, ok, err := qs.Get(context.Background(), "user", owner)
	if err != nil || !ok || stored.MonthlyTokens != 500000 {
		t.Fatalf("store after put: ok=%v err=%v budget=%+v", ok, err, stored)
	}

	// Clear it; a second clear is then a 404 (there is nothing left to remove).
	if rec := e.as(admin, "DELETE", "/api/admin/quotas?scope=user&owner_id="+owner, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("clear = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if rec := e.as(admin, "DELETE", "/api/admin/quotas?scope=user&owner_id="+owner, nil); rec.Code != http.StatusNotFound {
		t.Errorf("clear of an already-cleared budget = %d, want 404", rec.Code)
	}
}

func TestQuotaSetValidatesInput(t *testing.T) {
	e, _ := quotaEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad scope", map[string]any{"scope": "planet", "owner_id": "x", "monthly_tokens": 1000}},
		{"missing owner", map[string]any{"scope": "user", "monthly_tokens": 1000}},
		{"zero tokens", map[string]any{"scope": "user", "owner_id": "x", "monthly_tokens": 0}},
		{"negative tokens", map[string]any{"scope": "user", "owner_id": "x", "monthly_tokens": -1}},
	}
	for _, c := range cases {
		if rec := e.as(admin, "PUT", "/api/admin/quotas", c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d (%s), want 400", c.name, rec.Code, rec.Body.String())
		}
	}
}
