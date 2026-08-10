package adminapi

import (
	"net/http"
	"testing"
)

// Service-key routes are platform-admin acts: the matrix is {outsider, admin}.
// The full flow (create → returned token authenticates → revoke → token dead)
// lives in the identity package's store/service tests; here we pin the route
// surface and the admin-only guard.

func TestServiceKeysRequireAdmin(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin")
	regular := e.user("user")

	// A regular account is denied on every service-key route.
	e.actor = regular
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/admin/service-keys", nil},
		{"POST", "/api/admin/service-keys", map[string]any{"name": "x", "user_id": admin.ID}},
		{"DELETE", "/api/admin/service-keys/00000000-0000-0000-0000-000000000000", nil},
	} {
		rec := e.as(regular, req.method, req.path, req.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as regular: status %d, want 403", req.method, req.path, rec.Code)
		}
	}

	// Admin: list works (empty), create returns the token once, revoke 204.
	e.actor = admin
	rec := e.as(admin, "GET", "/api/admin/service-keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list as admin: %d", rec.Code)
	}

	rec = e.as(admin, "POST", "/api/admin/service-keys", map[string]any{"name": "ci-bot", "user_id": admin.ID, "ttl_days": 0})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create as admin: %d (%s)", rec.Code, rec.Body)
	}
	key := decodeBody(t, rec)["service_key"].(map[string]any)
	id := key["id"].(string)
	token := key["token"].(string)
	if token == "" || token[:3] != "sk_" {
		t.Fatalf("create did not return an sk_ token: %q", token)
	}
	if key["name"] != "ci-bot" || key["user_id"] != admin.ID {
		t.Fatalf("create payload mismatch: %v", key)
	}

	rec = e.as(admin, "DELETE", "/api/admin/service-keys/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke as admin: %d (%s)", rec.Code, rec.Body)
	}

	// Revoking an unknown id is a 404.
	rec = e.as(admin, "DELETE", "/api/admin/service-keys/00000000-0000-0000-0000-000000000000", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown id: %d, want 404", rec.Code)
	}
}

func TestCreateServiceKeyValidation(t *testing.T) {
	e := newEnv(t)
	e.actor = e.user("admin")
	u := e.user("user")

	for _, body := range []map[string]any{
		{},
		{"name": "", "user_id": u.ID},
		{"name": "x", "user_id": ""},
		{"name": "x", "user_id": u.ID, "ttl_days": -1},
	} {
		rec := e.as(e.actor, "POST", "/api/admin/service-keys", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, rec.Code)
		}
	}
}
