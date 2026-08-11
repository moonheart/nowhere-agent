package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/settings"
	"nowhere-agent/internal/usage"
)

// settingsEnv wires the admin handler with a real settings Runtime over the
// dev DB, so PUT→GET round trips are exercised end to end.
func settingsEnv(t *testing.T) (*env, *settings.Runtime) {
	t.Helper()
	e := newEnv(t)
	store := settings.NewStore(e.db)
	rt := settings.NewRuntime(store, map[string]json.RawMessage{
		settings.KeyWebhookURL: mustMarshal(t, "https://default.example/hook"),
	}, nil)
	if err := rt.Load(context.Background()); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	// Rebuild the handler with the settings runtime wired (same fake-auth
	// mux shape as newEnv).
	e.mux = http.NewServeMux()
	authed := httpx.NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), e.actor)))
		})
	})
	NewHandler(e.svc, usage.NewStore(e.db), e.mem).
		WithRuntimeSettings(rt).
		RegisterAuthed(authed)
	authed.Mount(e.mux, "/api/")
	return e, rt
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSettingsRequireAdmin(t *testing.T) {
	e, _ := settingsEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	regular := e.user(identity.PlatformRoleUser)

	if rec := e.as(regular, "GET", "/api/admin/settings", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("list as regular: %d, want 403", rec.Code)
	}
	if rec := e.as(regular, "PUT", "/api/admin/settings/"+settings.KeyWebhookURL, map[string]any{"value": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("put as regular: %d, want 403", rec.Code)
	}
	_ = admin
}

func TestSettingsRoundTrip(t *testing.T) {
	e, rt := settingsEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	key := settings.KeyWebhookURL
	t.Cleanup(func() {
		rt.Set(context.Background(), key, json.RawMessage("null"))
	})

	// The env default is in effect.
	rec := e.as(admin, "GET", "/api/admin/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	body := decodeBody(t, rec)
	list := body["settings"].([]any)
	found := false
	for _, s := range list {
		smap := s.(map[string]any)
		if smap["key"] == key {
			found = true
			if smap["value"] != "https://default.example/hook" {
				t.Fatalf("default value = %v", smap["value"])
			}
		}
	}
	if !found {
		t.Fatal("webhook_url key not listed")
	}

	// Override; the runtime applies it immediately (no restart).
	rec = e.as(admin, "PUT", "/api/admin/settings/"+key, map[string]any{"value": "https://ops.example/hook"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put: %d (%s)", rec.Code, rec.Body)
	}
	if got := rt.String(key); got != "https://ops.example/hook" {
		t.Fatalf("runtime did not apply override: %q", got)
	}

	// Unknown key is refused.
	rec = e.as(admin, "PUT", "/api/admin/settings/nope", map[string]any{"value": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("put unknown: %d, want 404", rec.Code)
	}

	// Clearing (null) returns to the env default.
	rec = e.as(admin, "PUT", "/api/admin/settings/"+key, map[string]any{"value": nil})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d", rec.Code)
	}
	if got := rt.String(key); got != "https://default.example/hook" {
		t.Fatalf("cleared setting did not fall back: %q", got)
	}
}

func TestSettingsIntKeys(t *testing.T) {
	e, rt := settingsEnv(t)
	admin := e.user(identity.PlatformRoleAdmin)
	key := settings.KeyRateLimitRPS
	t.Cleanup(func() {
		rt.Set(context.Background(), key, json.RawMessage("null"))
	})

	rec := e.as(admin, "PUT", "/api/admin/settings/"+key, map[string]any{"value": 30})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put: %d (%s)", rec.Code, rec.Body)
	}
	if got := rt.Int(key); got != 30 {
		t.Fatalf("runtime int = %d, want 30", got)
	}
}
