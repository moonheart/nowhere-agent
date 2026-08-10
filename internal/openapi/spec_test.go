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
