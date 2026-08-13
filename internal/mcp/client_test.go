package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"nowhere-agent/internal/toolruntime"
)

// echoArgs is the input for the test echo tool.
type echoArgs struct {
	Text string `json:"text" jsonschema:"text to echo back"`
}

// newTestServer returns an httptest server hosting an MCP server with two
// tools: "echo" (returns its text argument) and "fail" (returns an IsError
// tool result). The cleanup closes the server.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test-mcp", Version: "v0.0.1"}, nil)

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "echo",
		Description: "echo back the provided text",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in echoArgs) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo:" + in.Text}},
		}, nil, nil
	})

	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "fail",
		Description: "always returns a tool-level error",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in echoArgs) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "boom: " + in.Text}},
			IsError: true,
		}, nil, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)
	return hs
}

// connectClient builds a Client for the test server and connects it.
func connectClient(t *testing.T, url string) *Client {
	t.Helper()
	c := New("searxng", url, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c
}

func TestConnectListsAndAdaptsTools(t *testing.T) {
	hs := newTestServer(t)
	c := connectClient(t, hs.URL)

	tools := c.Tools()
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	byName := map[string]bool{}
	for _, tool := range tools {
		byName[tool.Name()] = true
	}
	if !byName["mcp_searxng_echo"] || !byName["mcp_searxng_fail"] {
		t.Errorf("expected mcp_searxng_echo and mcp_searxng_fail, got %v", byName)
	}
}

func TestToolPrefixDescriptionRiskSchema(t *testing.T) {
	hs := newTestServer(t)
	c := connectClient(t, hs.URL)

	var echo toolruntime.Tool
	for _, tool := range c.Tools() {
		if tool.Name() == "mcp_searxng_echo" {
			echo = tool
		}
	}
	if echo == nil {
		t.Fatal("mcp_searxng_echo not adapted")
	}
	if echo.Description() != "echo back the provided text" {
		t.Errorf("description not passed through: %q", echo.Description())
	}
	if echo.Risk() != toolruntime.RiskNetwork {
		t.Errorf("risk = %q, want RiskNetwork", echo.Risk())
	}
	schema := echo.Schema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties missing/not object: %v", schema)
	}
	if _, hasText := props["text"]; !hasText {
		t.Errorf("schema missing 'text' property: %v", props)
	}
}

func TestCallReturnsText(t *testing.T) {
	hs := newTestServer(t)
	c := connectClient(t, hs.URL)

	res := c.call(context.Background(), "echo", map[string]any{"text": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Content)
	}
	if res.Content != "echo:hello" {
		t.Errorf("got %q, want echo:hello", res.Content)
	}
}

func TestCallToolErrorSurfaced(t *testing.T) {
	hs := newTestServer(t)
	c := connectClient(t, hs.URL)

	res := c.call(context.Background(), "fail", map[string]any{"text": "x"})
	if !res.IsError {
		t.Fatalf("expected IsError result, got %+v", res)
	}
	if !strings.Contains(res.Content, "boom") {
		t.Errorf("error content %q should carry server text", res.Content)
	}
}

func TestClientCloseReleasesSession(t *testing.T) {
	hs := newTestServer(t)
	c := connectClient(t, hs.URL)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		t.Error("session must be nil after Close")
	}
	if len(c.tools) != 0 {
		t.Errorf("tools must be cleared after Close, got %d", len(c.tools))
	}
}

func TestReconnectAfterSessionDrop(t *testing.T) {
	hs := newTestServer(t)
	c := connectClient(t, hs.URL)

	// Simulate the server forgetting the session: close the client session so a
	// subsequent call sees a connection error and triggers reconnect+retry.
	c.mu.Lock()
	if c.session != nil {
		_ = c.session.Close()
	}
	c.mu.Unlock()

	res := c.call(context.Background(), "echo", map[string]any{"text": "again"})
	if res.IsError {
		t.Fatalf("expected reconnect+retry to succeed, got error: %q", res.Content)
	}
	if res.Content != "echo:again" {
		t.Errorf("got %q, want echo:again", res.Content)
	}
}

func TestSchemaFromFallback(t *testing.T) {
	if got := schemaFrom(nil); got["type"] != "object" {
		t.Errorf("nil schema fallback = %v, want object", got)
	}
	if got := schemaFrom("not-a-map"); got["type"] != "object" {
		t.Errorf("non-map schema fallback = %v, want object", got)
	}
	in := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	if got := schemaFrom(in); got["type"] != "object" {
		t.Errorf("map schema not passed through: %v", got)
	} else if _, ok := got["properties"].(map[string]any); !ok {
		t.Errorf("map schema properties lost: %v", got)
	}
}
