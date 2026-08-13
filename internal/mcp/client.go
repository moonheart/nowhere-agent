// Package mcp integrates external MCP (Model Context Protocol) tool servers
// into the agent loop. It speaks the Streamable HTTP transport only (no stdio,
// no legacy SSE) and adapts each remote tool to toolruntime.Tool, so the loop
// never depends on a tool being remote (the tool-runtime "MCP seam").
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"nowhere-agent/internal/toolruntime"
)

// searxngServer is the fixed server name for the built-in SearXNG integration.
// It prefixes every adapted tool name (mcp_searxng_<tool>).
const searxngServer = "searxng"

// defaultTimeout bounds a single MCP tool call when the server config sets none.
const defaultTimeout = 30 * time.Second

// Client owns one MCP server's session and exposes its tools as
// toolruntime.Tool values. Construct it with New, then Connect to perform the
// handshake and list tools. Safe for concurrent use.
type Client struct {
	server   string
	endpoint string
	hc       *http.Client
	timeout  time.Duration

	mu      sync.Mutex
	session *sdkmcp.ClientSession
	tools   []toolruntime.Tool
}

// New creates a Client for an MCP server at endpoint (a Streamable HTTP URL).
// server is the short name used to prefix adapted tool names. timeout bounds a
// single tool call; zero uses a default.
func New(server, endpoint string, timeout time.Duration) *Client {
	return NewWithHeaders(server, endpoint, timeout, nil)
}

// NewWithHeaders is New with per-request headers (bearer tokens, api keys,
// custom tracing headers an enterprise MCP server requires).
func NewWithHeaders(server, endpoint string, timeout time.Duration, headers map[string]string) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		server:   server,
		endpoint: endpoint,
		hc:       &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: headers}},
		timeout:  timeout,
	}
}

// NewSearxng creates a Client for the built-in SearXNG MCP integration.
func NewSearxng(endpoint string, timeout time.Duration) *Client {
	return New(searxngServer, endpoint, timeout)
}

// Server returns the server name used to prefix tool names.
func (c *Client) Server() string { return c.server }

// Close releases the client's MCP session (the SDK closes the transport's HTTP
// stream with it) and drops the adapted tool set. The request transport wraps
// the process-wide http.DefaultTransport, so idle HTTP connections are left
// for the shared pool — there is nothing else to tear down. Close is
// idempotent and safe to call when never connected; the Manager calls it for
// dropped and rebuilt servers, never to shut the process down.
func (c *Client) Close() error {
	c.mu.Lock()
	session := c.session
	c.session = nil
	c.tools = nil
	c.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

// sameConfig reports whether this client's wiring (endpoint, timeout,
// headers) matches a desired config — the Manager keeps a client whose
// config is unchanged across a runtime reconfigure, so its live session and
// tools survive an unrelated edit elsewhere in the server list. The timeout
// comparison normalizes the empty value to the construction default, exactly
// as NewWithHeaders does.
func (c *Client) sameConfig(want ServerConfig) bool {
	wantTimeout := want.timeoutFor()
	if wantTimeout <= 0 {
		wantTimeout = defaultTimeout
	}
	if c.server != want.Name || c.endpoint != want.URL || c.timeout != wantTimeout {
		return false
	}
	t, ok := c.hc.Transport.(headerTransport)
	if !ok {
		return len(want.Headers) == 0
	}
	return reflect.DeepEqual(t.headers, want.Headers)
}

// headerTransport injects fixed per-request headers (bearer tokens, api keys)
// into every request a Client makes. It is used when the server config carries
// headers; nil headers is a plain passthrough.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(req)
}

// Connect performs the initialize handshake and lists the server's tools,
// building the adapted toolruntime.Tool set. It must be called before Tools or
// Call. Re-calling Connect replaces the session (used for reconnect).
func (c *Client) Connect(ctx context.Context) error {
	session, err := c.dial(ctx)
	if err != nil {
		return err
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		return fmt.Errorf("mcp %s: list tools: %w", c.server, err)
	}
	tools := make([]toolruntime.Tool, 0, len(res.Tools))
	for _, rt := range res.Tools {
		tools = append(tools, c.adapt(rt))
	}
	c.mu.Lock()
	c.session = session
	c.tools = tools
	c.mu.Unlock()
	return nil
}

// dial establishes a new initialized session over Streamable HTTP.
func (c *Client) dial(ctx context.Context) (*sdkmcp.ClientSession, error) {
	transport := &sdkmcp.StreamableClientTransport{
		Endpoint:   c.endpoint,
		HTTPClient: c.hc,
		// Request/response only: this client consumes no server-initiated
		// traffic, so no standalone SSE stream is established.
		DisableStandaloneSSE: true,
		MaxRetries:           0, // reconnect is handled at the call layer (D2)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    "nowhere-agent",
		Version: "0.1.0",
	}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: connect %s: %w", c.server, c.endpoint, err)
	}
	return session, nil
}

// Tools returns the adapted tools built by the most recent Connect.
func (c *Client) Tools() []toolruntime.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]toolruntime.Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// call invokes a remote tool by its remote (unprefixed) name, mapping the
// result to a toolruntime.Result. On a dropped/closed session it reconnects
// once and retries once before surfacing an error result.
func (c *Client) call(ctx context.Context, remoteName string, args map[string]any) toolruntime.Result {
	res, err := c.callTool(ctx, remoteName, args)
	if err != nil && isSessionError(err) {
		// The session died (server restart, idle reap). Re-establish and retry once.
		if rerr := c.reconnect(ctx); rerr == nil {
			res, err = c.callTool(ctx, remoteName, args)
		}
	}
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("mcp tool %s failed: %v", remoteName, err), IsError: true}
	}
	return resultFrom(res)
}

// callTool performs a single tools/call against the current session.
func (c *Client) callTool(ctx context.Context, remoteName string, args map[string]any) (*sdkmcp.CallToolResult, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return nil, errors.New("not connected")
	}
	return session.CallTool(ctx, &sdkmcp.CallToolParams{Name: remoteName, Arguments: args})
}

// reconnect replaces the session (and refreshed tool set) after a failure.
func (c *Client) reconnect(ctx context.Context) error {
	c.mu.Lock()
	old := c.session
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return c.Connect(ctx)
}

// resultFrom maps an MCP CallToolResult to a toolruntime.Result: text content
// is joined, structured content is appended as JSON when present, and IsError
// is honored so the model sees tool failures and can self-correct.
func resultFrom(res *sdkmcp.CallToolResult) toolruntime.Result {
	var text string
	for _, content := range res.Content {
		if tc, ok := content.(*sdkmcp.TextContent); ok {
			text += tc.Text
		}
	}
	if text == "" && res.StructuredContent != nil {
		text = fmt.Sprintf("%v", res.StructuredContent)
	}
	if text == "" {
		text = "(no output)"
	}
	return toolruntime.Result{Content: text, IsError: res.IsError}
}

// isSessionError reports whether err indicates a dead session worth a reconnect.
func isSessionError(err error) bool {
	return errors.Is(err, sdkmcp.ErrConnectionClosed)
}

// adapt converts a remote MCP tool definition into a toolruntime.Tool named
// mcp_<server>_<remote>.
func (c *Client) adapt(rt *sdkmcp.Tool) toolruntime.Tool {
	return &mcpTool{
		client:     c,
		server:     c.server,
		remoteName: rt.Name,
		desc:       rt.Description,
		schema:     schemaFrom(rt.InputSchema),
		timeout:    c.timeout,
	}
}

// schemaFrom coerces a remote input schema (which the SDK unmarshals to a
// map[string]any) into the loop's schema shape, falling back to a permissive
// object schema when it cannot be interpreted.
func schemaFrom(in any) map[string]any {
	if m, ok := in.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{"type": "object"}
}
