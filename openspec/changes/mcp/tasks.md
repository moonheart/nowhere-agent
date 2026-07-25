# mcp — tasks

Standing constraints: write `*_test.go` for all Go code and run `go test ./...` green before
commit. No `Co-Authored-By` trailer.

## 1. MCP client (internal/mcp)

- [x] 1.1 `Client` wrapping the SDK: `StreamableClientTransport{Endpoint, HTTPClient, DisableStandaloneSSE:true}`; `Connect` (handshake) + `ListTools` building the adapted tools; mutex-guarded session. — `internal/mcp/client.go`
- [x] 1.2 `call(ctx, remoteName, args)`: `session.CallTool` with `CallToolParams{Name, Arguments}`; on transport/session error reconnect once + retry once; join `*mcp.TextContent`; honor `CallToolResult.IsError`. — `internal/mcp/client.go`
- [x] 1.3 `NewSearxng(url)` constructor binding server name `"searxng"`. — `internal/mcp/client.go`

## 2. Tool adapter (internal/mcp)

- [x] 2.1 `mcpTool` implements `toolruntime.Tool`: `Name()="mcp_<server>_<remote>"`, `Description()` passthrough, `Schema()` = `InputSchema.(map[string]any)` with ok-guard fallback to `{"type":"object"}`, `Risk()=RiskNetwork`, `Timeout()` from client, `Call` → `client.call`. — `internal/mcp/tool.go`

## 3. Config (internal/config)

- [x] 3.1 `MCP` struct: `Enabled` (`MCP_ENABLED`, default false), `SearxngURL` (`MCP_SEARXNG_URL`, default `https://searxng-mcp.moonheart.dev/mcp`). — `internal/config/config.go`
- [x] 3.2 `.env.example`: document `MCP_ENABLED`, `MCP_SEARXNG_URL`.

## 4. Wiring (cmd/server)

- [x] 4.1 When `MCP_ENABLED`, build the searxng client and connect/list at startup (fail fast with a clear error if the handshake fails).
- [x] 4.2 Extend the `ToolBinder` to register MCP tools into the run's registry (the same registry instance passed to `NewSpawnTool`); run a binder when sandbox OR mcp is configured so MCP works with sandbox off.

## 5. Tests

- [x] 5.1 In-process MCP server via SDK (`mcp.NewServer` + `NewStreamableHTTPHandler` over `httptest`): tools listed & adapted; `mcp_<server>_` prefix; `Schema()` passthrough; `Call` returns text; remote `IsError` → `Result.IsError`; `Risk()==RiskNetwork`; malformed-schema fallback. — `internal/mcp/client_test.go`, `internal/mcp/tool_test.go`
- [x] 5.2 Reconnect-on-dropped-session: a call after the session is dropped succeeds via reconnect+retry. — `internal/mcp/client_test.go`
- [x] 5.3 Config defaults + override. — `internal/config/config_test.go`

## 6. Validation

- [x] 6.1 `openspec validate mcp --strict` passes.
- [x] 6.2 `go test ./...` green (all packages).
- [x] 6.3 Live smoke: `MCP_ENABLED=true`, ask the agent to search the web; observe a `mcp_searxng_web_search` tool call and a summarized answer; confirm a spawned subagent can call it.
