# tool-runtime — delta for mcp

## MODIFIED Requirements

### Requirement: Tool sources
The runtime SHALL support built-in tools, skill-provided (L2) scripts, and MCP tool servers
over the Streamable HTTP transport. Built-in tools are constructed per session and bound to
that session's sandbox, so a tool physically cannot address another session's files. MCP tools
are adapted to the same Tool interface (see the `mcp` capability), so the loop never depends on
a tool's origin.

#### Scenario: Built-in tools available
- **WHEN** a session runs
- **THEN** built-in file tools (`read_file`, `write_file`, `list_dir`) are registered into the loop's tool registry and callable, bound to the session's sandbox

#### Scenario: Skill script as tool
- **WHEN** a skill's L2 script is invoked
- **THEN** it is dispatched through the same Tool interface and executed in the sandbox

#### Scenario: MCP seam
- **WHEN** an MCP tool server is configured over Streamable HTTP
- **THEN** its tools are listed, adapted to the Tool interface, registered into the loop's tool registry, and callable without changing the loop
