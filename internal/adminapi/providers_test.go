package adminapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/usage"
)

// providerEnv is the console with the provider registry wired, for the
// provider-route authorization and behaviour tests below.
func providerEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	h := NewHandler(e.svc, usage.NewStore(e.db), e.mem).
		WithProviders(providerreg.NewPGStore(e.db)).
		WithAudit(audit.NewLogger(e.db, quietLogger()))
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

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// createSystemProvider creates one via the admin API and returns its id. The
// provider row is deleted on cleanup.
func (e *env) createSystemProvider(t *testing.T, admin identity.User) string {
	t.Helper()
	return e.createProvider(t, admin, "/api/admin/providers")
}

func (e *env) createProvider(t *testing.T, admin identity.User, path string) string {
	t.Helper()
	rec := e.as(admin, "POST", path, map[string]any{
		"name": "sys-" + randSuffix(), "vendor": "anthropic", "api_key": "sk-ant-test-secret-9999",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create provider = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var out struct {
		Provider providerDTO `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Cleanup(func() { e.db.Exec(`DELETE FROM providers WHERE id = $1`, out.Provider.ID) })
	return out.Provider.ID
}

func (e *env) createModelFor(t *testing.T, admin identity.User, providerID, name string) string {
	t.Helper()
	rec := e.as(admin, "POST", "/api/admin/providers/"+providerID+"/models", map[string]any{
		"name": name, "vision": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create model = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var out struct {
		Model modelDTO `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Cleanup(func() { e.db.Exec(`DELETE FROM provider_models WHERE id = $1`, out.Model.ID) })
	return out.Model.ID
}

// ---- platform tier ----

// The system-provider routes are platform-admin only: an ordinary account and
// a team owner (who is not a platform admin) must get 403.
func TestProviderRoutesRejectNonAdmins(t *testing.T) {
	e := providerEnv(t)
	ordinary := e.user(identity.PlatformRoleUser)
	admin := e.user(identity.PlatformRoleAdmin)

	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/admin/providers", nil},
		{"POST", "/api/admin/providers", map[string]any{"name": "x-" + randSuffix(), "vendor": "openai"}},
	} {
		if rec := e.as(ordinary, rt.method, rt.path, rt.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as non-admin = %d, want 403", rt.method, rt.path, rec.Code)
		}
		if rec := e.as(admin, rt.method, rt.path, rt.body); rec.Code >= 400 {
			t.Errorf("%s %s as admin = %d (%s), want success", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateProviderNeverReturnsPlaintextKey(t *testing.T) {
	e := providerEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	rec := e.as(admin, "POST", "/api/admin/providers", map[string]any{
		"name": "sec-" + randSuffix(), "vendor": "openai", "api_key": "sk-do-not-echo-7777",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "do-not-echo") {
		t.Errorf("response echoed the secret: %s", rec.Body.String())
	}
	var out struct {
		Provider providerDTO `json:"provider"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	t.Cleanup(func() { e.db.Exec(`DELETE FROM providers WHERE id = $1`, out.Provider.ID) })
}

func TestProviderVendorValidation(t *testing.T) {
	e := providerEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	rec := e.as(admin, "POST", "/api/admin/providers", map[string]any{
		"name": "bad-" + randSuffix(), "vendor": "nonesuch",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown vendor = %d, want 400", rec.Code)
	}
}

func TestSystemProviderCRUD(t *testing.T) {
	e := providerEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	id := e.createSystemProvider(t, admin)
	modelID := e.createModelFor(t, admin, id, "claude-test-1")

	// Listing shows the provider with its model and a masked key.
	rec := e.as(admin, "GET", "/api/admin/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var list struct {
		Providers []providerDTO `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *providerDTO
	for i := range list.Providers {
		if list.Providers[i].ID == id {
			found = &list.Providers[i]
		}
	}
	if found == nil {
		t.Fatal("created provider not in the listing")
	}
	if found.Key == "sk-ant-test-secret-9999" || found.Key == "" {
		t.Errorf("key not masked: %q", found.Key)
	}
	if len(found.Models) != 1 || found.Models[0].ID != modelID {
		t.Errorf("models = %+v, want the created model", found.Models)
	}

	// Promote the model to default, then the provider to platform default.
	if rec := e.as(admin, "POST", "/api/admin/providers/"+id+"/models/"+modelID+"/default", nil); rec.Code != http.StatusNoContent {
		t.Errorf("set default model = %d", rec.Code)
	}
	if rec := e.as(admin, "POST", "/api/admin/providers/"+id+"/default", nil); rec.Code != http.StatusNoContent {
		t.Errorf("set default provider = %d (%s)", rec.Code, rec.Body.String())
	}

	// Update the provider's name.
	rec = e.as(admin, "PATCH", "/api/admin/providers/"+id, map[string]any{"name": "renamed-" + randSuffix()})
	if rec.Code != http.StatusOK {
		t.Errorf("update = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// Deleting the platform default is rejected (the default must be cleared
	// first); the raw cleanup in the helper still removes the row afterwards.
	rec = e.as(admin, "DELETE", "/api/admin/providers/"+id, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("delete default provider = %d, want 409", rec.Code)
	}
}

// The platform default is exclusive: promoting a second provider clears the
// first.
func TestDefaultProviderSingle(t *testing.T) {
	e := providerEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	a := e.createSystemProvider(t, admin)
	b := e.createSystemProvider(t, admin)

	if rec := e.as(admin, "POST", "/api/admin/providers/"+a+"/default", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("default a = %d", rec.Code)
	}
	if rec := e.as(admin, "POST", "/api/admin/providers/"+b+"/default", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("default b = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := e.as(admin, "GET", "/api/admin/providers", nil)
	var list struct {
		Providers []providerDTO `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var defaults int
	for _, p := range list.Providers {
		if p.ID == a && p.IsDefault {
			t.Error("provider a still default after b promoted")
		}
		if p.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("default count = %d, want 1", defaults)
	}
}

// Fetching a provider's model list is a preview: the route answers with the
// names the provider's API reports (flagged registered vs new) and writes
// nothing. The base URL points at a fake /v1/models endpoint.
func TestFetchModelsIsPreviewOnly(t *testing.T) {
	e := providerEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)

	modelsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"data":[{"id":"fetch-a"},{"id":"fetch-b"}]}`))
	}))
	defer modelsSrv.Close()

	// Create the provider with the fake server as its base URL.
	rec := e.as(admin, "POST", "/api/admin/providers", map[string]any{
		"name": "fetch-" + randSuffix(), "vendor": "openai",
		"base_url": modelsSrv.URL + "/v1", "api_key": "sk-x",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Provider providerDTO `json:"provider"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	pid := created.Provider.ID
	t.Cleanup(func() { e.db.Exec(`DELETE FROM providers WHERE id = $1`, pid) })

	// Fetch: both are new (nothing registered yet).
	rec = e.as(admin, "POST", "/api/admin/providers/"+pid+"/models/fetch", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch = %d (%s)", rec.Code, rec.Body.String())
	}
	var fetch struct {
		Models []struct {
			Name       string `json:"name"`
			Registered bool   `json:"registered"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fetch); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(fetch.Models) != 2 || fetch.Models[0].Name != "fetch-a" || fetch.Models[0].Registered {
		t.Errorf("fetch = %+v", fetch.Models)
	}

	// The fetch itself wrote nothing.
	var count int
	if err := e.db.QueryRow(`SELECT count(*) FROM provider_models WHERE provider_id = $1`, pid).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("fetch registered %d models; fetching must not write", count)
	}

	// After registering fetch-a, a second fetch flags it as registered.
	e.createModelFor(t, admin, pid, "fetch-a")
	rec = e.as(admin, "POST", "/api/admin/providers/"+pid+"/models/fetch", nil)
	json.Unmarshal(rec.Body.Bytes(), &fetch)
	byName := map[string]bool{}
	for _, m := range fetch.Models {
		byName[m.Name] = m.Registered
	}
	if !byName["fetch-a"] {
		t.Errorf("fetch-a not flagged registered after adding it: %+v", fetch.Models)
	}
}

// ---- team tier ----

func TestTeamProviderManagementAndAssignment(t *testing.T) {
	e := providerEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	owner := e.user(identity.PlatformRoleUser)
	member := e.user(identity.PlatformRoleUser)
	outsider := e.user(identity.PlatformRoleUser)
	tm := e.team(owner)
	e.join(tm, member, identity.RoleMember)
	// A second team for the outsider, so the "not a member" 404 is a real check.
	e.team(outsider)

	// Platform admin seeds a system provider; the team later assigns it.
	sysID := e.createSystemProvider(t, admin)
	sysModel := e.createModelFor(t, admin, sysID, "gpt-team-1")

	// A team member may read the team's visible providers but not manage them.
	if rec := e.as(member, "GET", "/api/teams/"+tm.ID+"/providers", nil); rec.Code != http.StatusOK {
		t.Errorf("member list = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if rec := e.as(member, "POST", "/api/teams/"+tm.ID+"/providers", map[string]any{"name": "x", "vendor": "openai"}); rec.Code != http.StatusForbidden {
		t.Errorf("member create = %d, want 403", rec.Code)
	}

	// A non-member gets 404 (team existence is hidden).
	if rec := e.as(outsider, "GET", "/api/teams/"+tm.ID+"/providers", nil); rec.Code != http.StatusNotFound {
		t.Errorf("outsider list = %d, want 404", rec.Code)
	}

	// Team admin creates a team-owned provider.
	rec := e.as(owner, "POST", "/api/teams/"+tm.ID+"/providers", map[string]any{
		"name": "team-" + randSuffix(), "vendor": "openai", "api_key": "sk-team-secret-1234",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create team provider = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var teamProv struct {
		Provider providerDTO `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &teamProv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	teamID := teamProv.Provider.ID
	t.Cleanup(func() { e.db.Exec(`DELETE FROM providers WHERE id = $1`, teamID) })
	if teamProv.Provider.Scope != "team" || teamProv.Provider.TeamID != tm.ID {
		t.Errorf("team provider scope = %+v", teamProv.Provider)
	}

	// Assign the system provider (visible to the team) with its model.
	rec = e.as(owner, "PUT", "/api/teams/"+tm.ID+"/provider-assignment", map[string]any{
		"provider_id": sysID, "model_id": sysModel,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("assign = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { e.db.Exec(`DELETE FROM team_provider_settings WHERE team_id = $1`, tm.ID) })

	// The team listing shows the assignment.
	rec = e.as(owner, "GET", "/api/teams/"+tm.ID+"/providers", nil)
	var listing struct {
		Assignment *struct {
			ProviderID string `json:"provider_id"`
			ModelID    string `json:"model_id"`
		} `json:"assignment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if listing.Assignment == nil || listing.Assignment.ProviderID != sysID || listing.Assignment.ModelID != sysModel {
		t.Errorf("assignment = %+v, want provider %s model %s", listing.Assignment, sysID, sysModel)
	}

	// A team can assign its own team-owned provider too.
	rec = e.as(owner, "PUT", "/api/teams/"+tm.ID+"/provider-assignment", map[string]any{
		"provider_id": teamID,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("assign own team provider = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// Clearing the assignment returns the team to the platform default.
	if rec := e.as(owner, "DELETE", "/api/teams/"+tm.ID+"/provider-assignment", nil); rec.Code != http.StatusNoContent {
		t.Errorf("clear assignment = %d", rec.Code)
	}
}
