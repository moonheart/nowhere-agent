package anthropic

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
	events, err := a.Stream(provider.Request{Model: "m", MaxTokens: 8, Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}})
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

// TestAdapterStreamHTTPError verifies non-200 responses surface an error.
func TestAdapterStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	a := New("k", WithEndpoint(srv.URL))
	_, err := a.Stream(provider.Request{Model: "m", MaxTokens: 8})
	if err == nil {
		t.Fatal("expected error for 429")
	}
}

func TestAdapterName(t *testing.T) {
	if New("k").Name() != "anthropic" {
		t.Error("wrong name")
	}
}
