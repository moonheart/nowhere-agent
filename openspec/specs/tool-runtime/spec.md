# tool-runtime Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Unified tool interface
The system SHALL expose all tools to the agent loop through a single Tool interface regardless of origin (built-in, skill script, or external MCP server). The loop SHALL NOT depend on a tool's origin.

#### Scenario: Origin-agnostic dispatch
- **WHEN** the loop dispatches a tool call
- **THEN** it uses the same Tool contract whether the tool is built-in, from a skill, or from an MCP server

### Requirement: Tool sources
The runtime SHALL support built-in tools, skill-provided (L2) scripts, and a seam for MCP tool servers. Built-in tools are constructed per session and bound to that session's sandbox, so a tool physically cannot address another session's files.

#### Scenario: Built-in tools available
- **WHEN** a session runs
- **THEN** built-in file tools (`read_file`, `write_file`, `list_dir`) are registered into the loop's tool registry and callable, bound to the session's sandbox

#### Scenario: Skill script as tool
- **WHEN** a skill's L2 script is invoked
- **THEN** it is dispatched through the same Tool interface and executed in the sandbox

#### Scenario: MCP seam
- **WHEN** an MCP tool server is configured
- **THEN** its tools are exposed through the Tool interface without changing the loop

### Requirement: Schema delivery
Each tool SHALL provide a machine-readable schema delivered to the model in the provider's function-calling format.

#### Scenario: Schema in request
- **WHEN** the loop assembles a model request
- **THEN** each available tool's schema is included in the provider-native tool-definition format

### Requirement: Execution controls
Tool execution SHALL support timeout, cancellation, and concurrent calls within a single turn.

#### Scenario: Timeout
- **WHEN** a tool exceeds its configured timeout
- **THEN** it is cancelled and a timeout error is returned to the model

#### Scenario: Concurrent tool calls
- **WHEN** the model requests multiple tool calls in one turn
- **THEN** they may execute concurrently and all results are returned

### Requirement: Error feedback for self-correction
Tool errors, including stderr, SHALL be returned to the model as tool-results so it can self-correct.

#### Scenario: Error surfaced to model
- **WHEN** a tool fails
- **THEN** the error (and stderr where applicable) is appended as a tool-result block for the next model turn

### Requirement: Built-in tools are sandbox-bound and path-confined
Built-in file tools SHALL operate through the session's `sandbox.Port` and SHALL be confined to the session workspace. Any path that escapes the workspace (via `..`, an absolute path, or a symlink) SHALL be rejected.

#### Scenario: File read through the sandbox
- **WHEN** the model calls `read_file` with a workspace-relative path
- **THEN** the tool returns the file content from the session's sandbox via `sandbox.Port.ReadFile`

#### Scenario: File write through the sandbox
- **WHEN** the model calls `write_file` with a workspace-relative path and content
- **THEN** the tool persists the content via `sandbox.Port.WriteFile` and reports the result

#### Scenario: Path escape rejected
- **WHEN** a file tool is called with a path that escapes the session workspace (`..`, absolute path, or symlink)
- **THEN** the call is rejected and an error result is returned to the model

#### Scenario: Tool risk classification
- **WHEN** a built-in file tool declares its risk
- **THEN** `read_file` and `list_dir` are `read_only` and `write_file` is `sandbox_write`

### Requirement: Scoped tool registry view
The registry SHALL be able to produce a filtered view of its tools for a child
run, selecting by an allow list and/or removing a deny list and/or excluding
named tools, without mutating the parent registry. The parent registry's tool
set SHALL be unchanged by producing a scoped view.

#### Scenario: Allow-list view
- **WHEN** a scoped view is requested with an allow list
- **THEN** the view contains only the named tools that exist in the parent registry

#### Scenario: Deny-list view
- **WHEN** a scoped view is requested with a deny list
- **THEN** the named tools are absent from the view even if otherwise allowed

#### Scenario: Wildcard view
- **WHEN** a scoped view is requested with no allow list (or a wildcard)
- **THEN** the view contains all parent tools minus any denied or excluded ones

#### Scenario: Parent registry unaffected
- **WHEN** a scoped view is produced
- **THEN** the parent registry still returns its full, unfiltered tool set

