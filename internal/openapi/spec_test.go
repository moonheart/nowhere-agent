package openapi

import (
	"encoding/json"
	"testing"
)

func TestSpecIsValidOpenAPIJSON(t *testing.T) {
	raw, err := JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Errorf("openapi version = %v", doc["openapi"])
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		t.Fatal("paths missing")
	}
	if _, ok := doc["components"].(map[string]any); !ok {
		t.Fatal("components missing")
	}
}

// realRoutes is the wire-format contract as the handlers actually register it
// (cmd/server/main.go mounts every package's Register): each of these MUST be
// documented. The point is to catch a route rename in code that leaves the
// spec stale — the /api/me/agent-defs → /api/me/agentdefs drift this test was
// written for.
var realRoutes = []string{
	// identity (internal/identity/http.go, phonehttp.go)
	"/api/auth/login", "/api/auth/signup", "/api/auth/logout", "/api/auth/totp/verify",
	"/api/auth/phone/request-code", "/api/auth/phone/verify",
	// chatapi
	"/api/chat", "/api/chat/history", "/api/chat/resume", "/api/chat/cancel",
	"/api/chat/sessions", "/api/chat/sessions/{id}/active", "/api/chat/sessions/{id}/state",
	// agentdefapi
	"/api/me/agentdefs",
	// inbound
	"/api/inbound/{id}", "/api/me/inbound",
	// adminapi / scheduleapi / skillapi / agentdefapi self-service
	"/api/me/scheduled-tasks", "/api/me/scheduled-tasks/{id}/run",
	"/api/me/tokens", "/api/me/usage",
	"/api/me/skills",
	"/api/admin/service-keys", "/api/admin/quotas", "/api/admin/audit",
	// meta
	"/healthz", "/openapi.json",
}

func TestSpecCoversRealRoutes(t *testing.T) {
	raw, _ := JSON()
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	paths := doc["paths"].(map[string]any)
	for _, p := range realRoutes {
		if _, ok := paths[p]; !ok {
			t.Errorf("spec missing real route %s — route renamed in code? openapi/paths.go is stale", p)
		}
	}
	// Spellings no handler registers must be gone from the docs.
	for _, p := range []string{"/api/me/agent-defs", "/api/me/agent_defs"} {
		if _, ok := paths[p]; ok {
			t.Errorf("spec documents %s, which no handler registers", p)
		}
	}
}

// TestSpecChatImagesSchemaMatchesWireFormat pins the chat request's image wire
// format: images arrive as [{path, mediaType}] objects referencing files the
// client pre-uploaded to the session workspace (chatapi/request.go
// incomingImagePart), NOT as base64 strings.
func TestSpecChatImagesSchemaMatchesWireFormat(t *testing.T) {
	raw, _ := JSON()
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	chat, ok := schemas["ChatRequest"].(map[string]any)
	if !ok {
		t.Fatal("ChatRequest schema missing")
	}
	images, ok := chat["properties"].(map[string]any)["images"].(map[string]any)
	if !ok {
		t.Fatal("ChatRequest.images missing")
	}
	items, ok := images["items"].(map[string]any)
	if !ok {
		t.Fatal("ChatRequest.images items schema missing")
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("images items must be an object with properties")
	}
	if _, ok := props["path"]; !ok {
		t.Error("images items schema must carry a path property")
	}
	if _, ok := props["mediaType"]; !ok {
		t.Error("images items schema must carry a mediaType property")
	}
}

func TestSpecCoversCoreEmbeddingSurface(t *testing.T) {
	raw, _ := JSON()
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	paths := doc["paths"].(map[string]any)
	for _, p := range []string{
		"/api/auth/login", "/api/auth/signup",
		"/api/chat", "/api/chat/history", "/api/chat/sessions",
		"/api/me/scheduled-tasks", "/api/me/scheduled-tasks/{id}/run",
		"/api/admin/service-keys", "/api/admin/quotas", "/api/admin/audit",
	} {
		if _, ok := paths[p]; !ok {
			t.Errorf("spec missing path %s", p)
		}
	}
	// Every documented path must carry at least one response.
	for p, item := range paths {
		ops := item.(map[string]any)
		for method, op := range ops {
			responses, ok := op.(map[string]any)["responses"].(map[string]any)
			if !ok || len(responses) == 0 {
				t.Errorf("%s %s has no responses", method, p)
			}
		}
	}
}
