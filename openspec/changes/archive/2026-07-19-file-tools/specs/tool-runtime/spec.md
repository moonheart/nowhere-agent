# tool-runtime — delta for file-tools

## MODIFIED Requirements

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

## ADDED Requirements

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
