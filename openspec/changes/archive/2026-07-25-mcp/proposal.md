# mcp — proposal

## Why

The agent loop can read/write files in its sandbox and spawn subagents, but it cannot reach
the open web. Asking "what's the latest on X" or "read this URL" stalls: there is no tool
origin outside the session workspace. The `tool-runtime` spec has always reserved a seam for
external tool servers (`MCP seam`), but only built-in file tools exist — the seam is
unimplemented.

The **Model Context Protocol (MCP)** is the standard way to expose an external tool server.
Integrating an MCP client lets the loop call third-party tools through the *same*
`toolruntime.Tool` contract it already uses, with no change to the loop. The concrete first
server is a self-hosted **SearXNG** metasearch MCP
(`https://searxng-mcp.moonheart.dev/mcp`) which gives the agent web search, search
suggestions, instance introspection, and URL→markdown reading.

## What changes

- **New capability `mcp`**: an MCP **client** that connects to a Streamable-HTTP MCP server,
  performs the initialize handshake, lists the server's tools, and adapts each one into a
  `toolruntime.Tool` the loop can call. Streamable HTTP is the **only** transport supported
  (no stdio, no legacy SSE).
- **Tool adaptation**: each remote MCP tool becomes a `toolruntime.Tool` —
  `Name`/`Description`/`Schema` pass through from the server, `Risk` is `RiskNetwork` (every
  MCP call is egress), and `Call` maps to the MCP `tools/call`, returning text or an
  error tool-result for the model to self-correct.
- **Naming**: registered tool names are prefixed `mcp_<server>_<tool>` so tools from any MCP
  server are collision-proof and self-describing.
- **Session lifecycle**: the client owns the MCP session (Streamable HTTP `Mcp-Session-Id`)
  and reconnects transparently if a call finds the session dropped.
- **Wiring**: MCP tools register into the **per-run** registry via the existing `ToolBinder`
  seam, so subagents inherit them through the existing scoped registry view.
- **Config**: `MCP_ENABLED` (default off) and `MCP_SEARXNG_URL` (default the moonheart
  instance) — v1 hardcodes the searxng integration rather than a generic server list.

## Capabilities touched

- `mcp` — **new** capability (connect/handshake, list→adapt, streamable-http-only, risk,
  naming, lifecycle/reconnect, error-to-result).
- `tool-runtime` — **modified**: the existing `MCP seam` scenario flips from "a seam exists"
  to "implemented over Streamable HTTP". The loop still does not depend on a tool's origin;
  no other tool-runtime requirement changes.

## Non-goals (deferred)

- **stdio / legacy SSE transports** — Streamable HTTP only, per requirement.
- **Generic multi-server config** (`MCP_SERVERS` JSON list) — v1 hardcodes searxng; the
  `Client`/`mcpTool` abstraction stays generic enough to add servers later without rework.
- **MCP server** (exposing nowhere-agent's tools *to* other MCP clients).
- **MCP resources / prompts**, OAuth authorization, and structured-content rendering — v1
  surfaces tools only, text results only.
- **Per-user MCP configuration** — servers are process-level config.
- **Approval UX enforcement of `RiskNetwork`** — the risk classification is declared and the
  permission layer maps it to `DecisionAsk`, but wiring `permission.Checker` into the dispatch
  path remains a separately-tracked task.
