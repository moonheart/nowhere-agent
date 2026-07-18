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
The runtime SHALL support built-in tools, skill-provided (L2) scripts, and a seam for MCP tool servers.

#### Scenario: Built-in tools available
- **WHEN** a session runs
- **THEN** built-in tools (e.g., file/command/web) are registered and callable

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

