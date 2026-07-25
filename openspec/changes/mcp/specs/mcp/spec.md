# mcp — spec (ADDED)

## ADDED Requirements

### Requirement: Streamable-HTTP MCP client
The system SHALL connect to an external MCP server over the Streamable HTTP transport only,
perform the MCP initialize handshake, and hold the resulting session for tool calls. No stdio
or legacy SSE transport SHALL be used. The client SHALL request request/response operation
(without a standalone server-initiated SSE stream) since it consumes no server-initiated
traffic.

#### Scenario: Handshake establishes a session
- **WHEN** the client connects to a configured MCP server endpoint
- **THEN** it completes the initialize handshake over Streamable HTTP and holds a session usable for tool calls

#### Scenario: Request/response transport
- **WHEN** the client establishes its session
- **THEN** it does not rely on a standalone server-initiated SSE stream, using request/response only

### Requirement: Remote tools adapted to the Tool interface
The system SHALL list the connected server's tools and expose each one to the agent loop as a
`toolruntime.Tool`, passing through the remote tool's description and input schema. The loop
SHALL NOT depend on the tool being remote.

#### Scenario: Server tools become loop tools
- **WHEN** the client has listed a server's tools
- **THEN** each remote tool is registered as a `toolruntime.Tool` whose description and input schema come from the server

#### Scenario: Schema passthrough
- **WHEN** a remote tool declares an input schema
- **THEN** the adapted tool's `Schema()` returns that schema so it is delivered to the model in the provider's function-calling format

#### Scenario: Malformed schema degrades safely
- **WHEN** a remote tool's input schema cannot be interpreted as an object schema
- **THEN** the adapted tool falls back to a permissive object schema rather than failing

### Requirement: MCP tool naming is prefixed
Each adapted tool SHALL be registered under a name prefixed with `mcp_<server>_` so tools from
any MCP server are collision-proof and self-describing.

#### Scenario: Prefixed registration
- **WHEN** a tool from server `searxng` named `searxng_web_search` is adapted
- **THEN** it is registered as `mcp_searxng_web_search`

### Requirement: MCP calls classified as network risk
Every adapted MCP tool SHALL declare `RiskNetwork`, since each call reaches outside the host.

#### Scenario: Risk classification
- **WHEN** an MCP tool's risk is read
- **THEN** it is `RiskNetwork`

### Requirement: Tool call maps to MCP tools/call
Calling an adapted tool SHALL invoke the server's `tools/call` with the call's arguments and
return the text content as the tool result. A tool-level error reported by the server SHALL be
surfaced as an error tool-result (not a crash) so the model can self-correct.

#### Scenario: Text result returned
- **WHEN** the server answers a tool call with text content
- **THEN** the adapted tool returns that text as its result content

#### Scenario: Server tool error surfaced
- **WHEN** the server reports a tool-level error for a call
- **THEN** the adapted tool returns an error tool-result carrying the error text for the model to self-correct

### Requirement: Session lifecycle with transparent reconnect
The client SHALL tolerate an expired or dropped MCP session by re-establishing the session and
retrying the call, so a long-lived process survives server restarts and idle reaping.

#### Scenario: Reconnect on dropped session
- **WHEN** a tool call fails because the MCP session is no longer valid
- **THEN** the client re-establishes the session and retries the call once before surfacing an error

### Requirement: SearXNG integration configured by environment
The system SHALL provide a built-in SearXNG MCP integration enabled by `MCP_ENABLED` (default
off) and targeting `MCP_SEARXNG_URL` (default the hosted instance), without requiring a generic
server-list configuration.

#### Scenario: Disabled by default
- **WHEN** `MCP_ENABLED` is unset
- **THEN** no MCP client is built and no MCP tools are registered

#### Scenario: Enabled registers searxng tools
- **WHEN** `MCP_ENABLED` is true
- **THEN** the client connects to `MCP_SEARXNG_URL` and its tools are registered into each run's tool registry
