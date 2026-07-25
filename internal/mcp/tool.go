package mcp

import (
	"context"
	"strings"
	"time"

	"nowhere-agent/internal/toolruntime"
)

// mcpTool adapts one remote MCP tool to toolruntime.Tool. Name, description,
// and schema pass through from the server; risk is always RiskNetwork (every
// call is egress). Tool-level failures are returned as error Results so the
// model can self-correct, never as Go errors that would crash the loop.
type mcpTool struct {
	client     *Client
	server     string
	remoteName string
	desc       string
	schema     map[string]any
	timeout    time.Duration
}

// Name returns the prefixed tool name mcp_<server>_<remote>. A redundant
// leading "<server>_" in the remote name is stripped so an already-prefixed
// server (e.g. searxng_web_search) doesn't double up (mcp_searxng_web_search,
// not mcp_searxng_searxng_web_search).
func (t *mcpTool) Name() string {
	remote := strings.TrimPrefix(t.remoteName, t.server+"_")
	return "mcp_" + t.server + "_" + remote
}

// Description returns the remote tool's description (carrying the server's own
// usage hints).
func (t *mcpTool) Description() string { return t.desc }

// Schema returns the remote tool's input schema.
func (t *mcpTool) Schema() map[string]any { return t.schema }

// Risk is always RiskNetwork: every MCP call reaches outside the host.
func (t *mcpTool) Risk() toolruntime.Risk { return toolruntime.RiskNetwork }

// Timeout bounds a single remote call.
func (t *mcpTool) Timeout() time.Duration { return t.timeout }

// Call invokes the remote tool via the client and maps the result.
func (t *mcpTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	return t.client.call(ctx, t.remoteName, args), nil
}
