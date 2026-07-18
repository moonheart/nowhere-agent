# Spec: sandbox

## ADDED Requirements

### Requirement: SandboxPort interface
The system SHALL define a SandboxPort interface exposing a minimal verb set: lifecycle (Create, Destroy), command execution (Exec), file operations (ReadFile, WriteFile, ListDir), and workspace materialize/solidify. The interface SHALL hide whether the backing sandbox is local Docker or a remote microVM.

#### Scenario: Backend-agnostic usage
- **WHEN** the agent loop or skill engine uses SandboxPort
- **THEN** it operates identically regardless of the configured sandbox backend

### Requirement: Per-session isolation
Sandbox lifetime SHALL be bound to a session; each session gets its own isolated sandbox.

#### Scenario: Session isolation
- **WHEN** two sessions are active
- **THEN** each has a distinct sandbox and neither can observe the other's processes or files

### Requirement: Built-in fs+Docker implementation
The built-in SandboxPort implementation SHALL isolate execution using filesystem isolation plus a Docker container.

#### Scenario: Command execution
- **WHEN** a command is executed in the sandbox
- **THEN** it runs inside the session's container with filesystem isolation from the host and other sessions

### Requirement: Deferred stop and scheduled destroy
On session end the sandbox SHALL stop after a configurable delay, and SHALL be destroyed by a scheduled reaper, while the workspace persists independently.

#### Scenario: Deferred stop
- **WHEN** a session ends
- **THEN** the sandbox remains resumable for the configured delay before being stopped

#### Scenario: Reactivation after destroy
- **WHEN** a session is reactivated after its sandbox was destroyed
- **THEN** a fresh sandbox is created and the persisted workspace is materialized into it

### Requirement: Network egress control
The sandbox SHALL enforce a network policy at the container layer (via an egress proxy/firewall), configured at creation. Modes SHALL include open, allowlist, and deny.

#### Scenario: Allowlist enforced
- **WHEN** a sandbox is created with an allowlist network policy
- **THEN** outbound connections to non-allowlisted hosts are blocked at the container layer even if code inside the sandbox initiates them

#### Scenario: Policy applied at creation
- **WHEN** execution-permission requires network gating
- **THEN** the sandbox is created with the corresponding NetworkPolicy so the restriction holds regardless of what runs inside

### Requirement: Remote sandbox seam
The interface SHALL permit a remote sandbox protocol implementation (e.g., gVisor/Firecracker) to be added without changing SandboxPort consumers.

#### Scenario: Pluggable backend
- **WHEN** a new sandbox backend is registered
- **THEN** it satisfies SandboxPort without modification to the loop, skill engine, or workspace logic
