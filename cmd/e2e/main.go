// Command e2e spins up a mock Anthropic SSE server and the real chat handler,
// then drives a full data-stream request through it, printing the SSE output.
// It verifies the end-to-end protocol path without real API keys.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/provider/anthropic"
	"nowhere-agent/internal/toolruntime"
)

func main() {
	// Mock Anthropic endpoint streaming a short answer.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello from nowhere-agent\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n")
	}))
	defer mock.Close()

	adapter := anthropic.New("test-key", anthropic.WithEndpoint(mock.URL))
	mux := http.NewServeMux()
	chatapi.NewHandler(func(ctx context.Context, system string) *agent.Loop {
		return agent.New(adapter, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}, "").Register(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/chat", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	fmt.Println("STATUS:", resp.Status)
	fmt.Println("CONTENT-TYPE:", resp.Header.Get("Content-Type"))
	fmt.Println("STREAM-HEADER:", resp.Header.Get("x-vercel-ai-ui-message-stream"))
	fmt.Println("--- BODY ---")
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}
