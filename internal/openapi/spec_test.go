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

// TestSpecRoutesMatchRegisteredRoutes pins the OpenAPI document to the
// routes the handlers actually register, in BOTH directions (see
// routes.go for the contract list):
//
//  1. every registered (path, method) must be documented — a route renamed
//     or added in code leaves the spec stale and this fails, and
//  2. every documented path must be registered — a spec-only route is a lie
//     and this fails.
func TestSpecRoutesMatchRegisteredRoutes(t *testing.T) {
	raw, _ := JSON()
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	paths := doc["paths"].(map[string]any)

	// Direction 1: registered ⊆ documented (per path and per method).
	for p, methods := range registeredRoutes {
		item, ok := paths[p].(map[string]any)
		if !ok {
			t.Errorf("spec missing registered route %s — route added or renamed? openapi/paths.go is stale", p)
			continue
		}
		for _, m := range methods {
			if _, ok := item[m]; !ok {
				t.Errorf("spec missing method %s on registered route %s — openapi/paths.go is stale", m, p)
			}
		}
	}

	// Direction 2: documented ⊆ registered. The "/" SPA catch-all is not an
	// API route and is intentionally never documented.
	for p := range paths {
		if _, ok := registeredRoutes[p]; !ok {
			t.Errorf("spec documents %s, which no handler registers — routes.go is stale", p)
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
