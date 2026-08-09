package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdapterModels verifies the GET /models list is decoded from a base URL
// configured as a /v1 root (the admin console's fetch-models action).
func TestAdapterModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"},{"id":"gpt-4-turbo"}]}`))
	}))
	defer srv.Close()

	a := New("test-key", WithEndpoint(srv.URL+"/v1"))
	names, err := a.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(names) != 3 || names[0] != "gpt-4o" || names[2] != "gpt-4-turbo" {
		t.Errorf("models = %v", names)
	}
}

// A legacy full endpoint (…/v1/chat/completions) normalizes to the same base,
// so the model list is still fetched from …/v1/models.
func TestAdapterModelsLegacyEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	a := New("k", WithEndpoint(srv.URL+"/v1/chat/completions"))
	if _, err := a.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
}
