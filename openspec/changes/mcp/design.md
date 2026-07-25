# mcp — design

## Grounding

The target server was probed live: `https://searxng-mcp.moonheart.dev/mcp` is
`ihor-sokoliuk/mcp-searxng` v1.11.1 speaking protocol `2025-06-18` over Streamable HTTP,
exposing 4 read-only tools (`searxng_web_search`, `searxng_search_suggestions`,
`searxng_instance_info`, `web_url_read`), each annotated `readOnlyHint + openWorldHint`.

We use the official Go SDK `github.com/modelcontextprotocol/go-sdk/mcp` (v1.6.1). Verified
API surface:

- `mcp.NewClient(&mcp.Implementation{Name,Version}, nil)` → `*Client`.
- `(&mcp.StreamableClientTransport{Endpoint, HTTPClient, DisableStandaloneSSE:true}).Connect(ctx)`.
- `client.Connect(ctx, transport, nil)` → `*ClientSession` (does the initialize handshake).
- `session.ListTools(ctx, nil)` → `*ListToolsResult` with `Tools []*mcp.Tool`.
- `session.CallTool(ctx, &mcp.CallToolParams{Name, Arguments: map[string]any})` → `*CallToolResult`.
- `mcp.Tool.InputSchema` is `any` and, from a server, holds a `map[string]any` — a direct fit
  for `toolruntime.Tool.Schema() map[string]any`.
- `CallToolResult.Content` is `[]mcp.Content` (interface); text is extracted by a type-switch on
  `*mcp.TextContent`. `CallToolResult.IsError bool` marks tool-level failure.
- `DisableStandaloneSSE: true` → pure request/response; no persistent GET stream. This server
  has no server-initiated traffic we consume, so request/response is correct and simpler.

## Key decisions

### D1: `mcpTool` adapts one remote tool to `toolruntime.Tool`

The origin-agnostic seam (`tool-runtime`) is the whole point: the loop never learns a tool is
remote. One `mcpTool` per remote tool:

- `Name()` = `mcp_<server>_<remote>` (D3).
- `Description()` = remote description (pass through; it carries the server's own usage hints,
  e.g. searxng's "use exactly `query`").
- `Schema()` = `InputSchema.(map[string]any)` with an ok-guard; on assertion failure fall back
  to `{"type":"object"}` so a malformed schema degrades rather than panics.
- `Risk()` = `toolruntime.RiskNetwork` — every MCP call leaves the host.
- `Timeout()` = the client's configured timeout (0 → registry default).
- `Call(ctx, args)` → `client.call(ctx, remoteName, args)`; returns
  `toolruntime.Result{Content: text}` or `{Content: msg, IsError: true}`. It never returns a Go
  error for a tool-level failure — the model must see the error to self-correct
  (`tool-runtime` "Error feedback for self-correction"). Go errors are reserved for the
  adapter itself being unusable.

### D2: `Client` owns the SDK session, with lazy reconnect

`Client` holds the endpoint, an `*http.Client`, the negotiated `*ClientSession`, and a mutex.
`Connect` does the handshake and one `ListTools`, building the `[]toolruntime.Tool` once (the
tool list is treated as stable for the process lifetime).

Streamable-HTTP sessions are identified by a server-issued `Mcp-Session-Id`; a long-lived
process can outlive it (server restart, idle reaping). So `call` is resilient: on a
transport/session error it re-establishes the session (`Connect`) once and retries the call
once. A retry that still fails returns an error tool-result. We do not re-list on reconnect
(tool set assumed stable); a changed tool set surfaces as an unknown-tool error from the
server, which the model sees.

### D3: Naming — always `mcp_<server>_<tool>`

Uniform, collision-proof across any present/future MCP server, and self-describing in the UI
and in agent-def allow-lists. With `server="searxng"` the registered names are
`mcp_searxng_web_search`, `mcp_searxng_search_suggestions`, `mcp_searxng_instance_info`,
`mcp_searxng_web_url_read`. The `Client` carries its server name; `mcpTool` names are built at
adapt time.

### D4: Per-run registration via the existing `ToolBinder`

Subagent inheritance is free *only if* MCP tools live in the same per-run registry that
`NewSpawnTool` scopes from (verified: `Registry.Scoped` copies every registered tool by name
regardless of origin). So the server startup builds the MCP `Client` once, and the `ToolBinder`
closure registers the client's tools into the run's registry alongside the file tools. Network
tools need no sandbox, so the binder must run when **sandbox OR mcp** is configured (today it
only runs when `sandboxMgr != nil`); when only MCP is on, the binder registers just MCP tools
(+ `spawn_agent` if enabled).

### D5: Hardcoded searxng, env only toggles/overrides

v1 config is `MCP_ENABLED` (default false) and `MCP_SEARXNG_URL` (default the moonheart URL).
Server name is the fixed `"searxng"`. The `Client`/`mcpTool` code stays generic (a `Client` is
built from `{name, url, timeout}`), so promoting to a multi-server list later is additive.

### D6: No UI / protocol change

MCP tools are ordinary `tool-call` parts: they stream args via the existing `tool-call-delta`,
render through the generic tool-call card, and appear in the Runs panel via `reportToolCall`.
Nothing in `web/` or the SSE protocol changes.

## Data flow

```
startup:  if MCP_ENABLED: mcpClient = mcp.NewSearxng(url); tools = mcpClient.Tools(ctx)  // connect + list
per run:  ToolBinder(loop, sessID):
            reg.Register(fileTools...)          // if sandbox on
            reg.Register(mcpClient.Tools()...)  // if mcp on   ← same registry
            reg.Register(spawn_agent(reg))      // if enabled
run:      model calls mcp_searxng_web_search{query}
            → Registry.Call → mcpTool.Call → client.call("searxng_web_search", args)
              → (reconnect+retry once on session drop) → CallToolResult
              → join *TextContent → Result{Content} (IsError honored)
subagent: spawn_agent scopes the SAME registry → child inherits mcp_* tools (wildcard defs)
```

## Testing strategy

Unit tests (`*_test.go`, green before commit), no external network:

- Spin an **in-process MCP server** with the same SDK (`mcp.NewServer` +
  `mcp.NewStreamableHTTPHandler` over `httptest.NewServer`) registering a couple of tools (a
  text-echo tool and a failing tool). Point the `Client` at the httptest URL.
- Assert: tools are listed and adapted; names carry the `mcp_<server>_` prefix; `Schema()`
  passes the server's `inputSchema` through; `Call` returns the text content; a remote
  `IsError` becomes `Result.IsError`; `Risk()` is `RiskNetwork`; description passes through.
- Reconnect: drop/expire the session server-side (or close the client session) and assert a
  subsequent `Call` still succeeds (reconnect+retry path).
- Schema edge: a tool whose `InputSchema` is not a `map[string]any` falls back to a permissive
  object schema without panicking.
- Config: `MCP_ENABLED`/`MCP_SEARXNG_URL` parse with defaults.
