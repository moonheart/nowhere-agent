package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nowhere-agent/internal/provider"
)

// TestAdapterStreamAgainstServer verifies headers are set and the SSE stream
// is decoded into canonical events, using a local test server.
func TestAdapterStreamAgainstServer(t *testing.T) {
	var gotAPIKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"))
		w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Yo\"}}\n\n"))
		w.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n"))
	}))
	defer srv.Close()

	a := New("test-key", WithEndpoint(srv.URL))
	events, err := a.Stream(context.Background(), provider.Request{Model: "m", MaxTokens: 8, Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var types []provider.EventType
	var lastText string
	for ev := range events {
		types = append(types, ev.Type)
		if ev.Type == provider.EventBlockDelta {
			lastText = ev.Delta
		}
	}

	if gotAPIKey != "test-key" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version not set")
	}
	if len(types) != 4 {
		t.Fatalf("got %d events: %v", len(types), types)
	}
	if lastText != "Yo" {
		t.Errorf("delta text = %q", lastText)
	}
}

// TestAdapterRecordsRawExchange verifies the recorder captures the raw request
// body and the streamed response bytes, and never the auth header.
func TestAdapterRecordsRawExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	a := New("secret-key", WithEndpoint(srv.URL), WithRawRecorder(provider.NewRawRecorder(dir)))
	events, err := a.Stream(context.Background(), provider.Request{Model: "m", MaxTokens: 8, Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range events { // drain to flush the recorded response
	}

	files, _ := filepath.Glob(filepath.Join(dir, "anthropic", "*.req"))
	if len(files) != 1 {
		t.Fatalf("expected 1 recorded request, got %v", files)
	}
	req, _ := os.ReadFile(files[0])
	if !strings.Contains(string(req), `"model":"m"`) {
		t.Errorf("recorded request missing body: %s", req)
	}
	if strings.Contains(string(req), "secret-key") {
		t.Error("auth material must never be recorded")
	}

	resp, _ := os.ReadFile(strings.TrimSuffix(files[0], ".req") + ".resp")
	if !strings.Contains(string(resp), "message_start") || !strings.Contains(string(resp), "output_tokens") {
		t.Errorf("recorded response missing SSE bytes: %s", resp)
	}
}

// TestAdapterStreamHTTPError verifies non-200 responses surface an error. Retry
// is disabled so the test targets classification, not backoff.
func TestAdapterStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	a := New("k", WithEndpoint(srv.URL), WithRetry(provider.RetryPolicy{MaxAttempts: 1}))
	_, err := a.Stream(context.Background(), provider.Request{Model: "m", MaxTokens: 8})
	if err == nil {
		t.Fatal("expected error for 429")
	}
}

// TestAdapterRetriesTransientStatus verifies a retryable status (503) is retried
// and the subsequent success streams normally.
func TestAdapterRetriesTransientStatus(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"))
		w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Yo\"}}\n\n"))
	}))
	defer srv.Close()

	a := New("k", WithEndpoint(srv.URL), WithRetry(provider.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}))
	events, err := a.Stream(context.Background(), provider.Request{Model: "m", MaxTokens: 8})
	if err != nil {
		t.Fatalf("Stream after retry: %v", err)
	}
	var lastText string
	for ev := range events {
		if ev.Type == provider.EventBlockDelta {
			lastText = ev.Delta
		}
	}
	if lastText != "Yo" {
		t.Errorf("delta text = %q, want Yo (stream should succeed on retry)", lastText)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server calls = %d, want 2 (one 503 + one success)", got)
	}
}

func TestAdapterName(t *testing.T) {
	if New("k").Name() != "anthropic" {
		t.Error("wrong name")
	}
}

// TestAdapterModels verifies the GET /models list is decoded from a base URL
// configured as a /v1 root (the admin console's fetch-models action).
func TestAdapterModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"data":[{"type":"model","id":"claude-3-5-sonnet-20241022","display_name":"Claude 3.5 Sonnet"},{"type":"model","id":"claude-3-haiku-20240307"}]}`))
	}))
	defer srv.Close()

	a := New("test-key", WithEndpoint(srv.URL+"/v1"))
	names, err := a.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(names) != 2 || names[0] != "claude-3-5-sonnet-20241022" || names[1] != "claude-3-haiku-20240307" {
		t.Errorf("models = %v", names)
	}
}

// A legacy full endpoint (…/v1/messages) normalizes to the same base, so the
// model list is still fetched from …/v1/models.
func TestAdapterModelsLegacyEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	a := New("k", WithEndpoint(srv.URL+"/v1/messages"))
	if _, err := a.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
}
