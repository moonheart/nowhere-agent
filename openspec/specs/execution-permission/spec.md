# execution-permission Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Two-layer permission model
The system SHALL separate resource permission (data ownership, handled by identity-scope) from execution permission (runtime authorization of agent actions).

#### Scenario: Independent concerns
- **WHEN** evaluating whether an agent action may proceed
- **THEN** execution permission is evaluated at runtime, independent of who owns the data

### Requirement: Permissive inside the sandbox
Actions contained within the session sandbox (running code, writing to the session workspace) SHALL be permitted by default.

#### Scenario: In-sandbox action allowed
- **WHEN** the agent runs a command or writes a file inside its session sandbox
- **THEN** the action proceeds without requiring user approval

### Requirement: Gate sandbox-escaping actions
Actions that reach outside the sandbox boundary SHALL be subject to a configurable approval policy. Escaping actions SHALL include network egress, writes outside the session workspace, and external/cost-incurring API calls.

#### Scenario: Network egress gated
- **WHEN** the agent attempts outbound network access beyond an allowlist
- **THEN** the approval policy is consulted before proceeding

#### Scenario: External write gated
- **WHEN** the agent attempts to write outside its session workspace
- **THEN** the approval policy is consulted before proceeding

### Requirement: Configurable approval policy
Approval policy SHALL be configurable per tool or risk level as allow, ask, or deny.

#### Scenario: Ask prompts the user
- **WHEN** a tool's policy is "ask" and the action is attempted
- **THEN** the user is prompted to approve or deny before execution

#### Scenario: Deny blocks the action
- **WHEN** a tool's policy is "deny"
- **THEN** the action is blocked and the model is informed

### Requirement: Approval flow and audit
Approval SHALL be one kind of a general **interaction** — a durable record with
an open `kind`, a `payload` shown to the client, and a `result` the client
returns. The resume path SHALL fold a resolved interaction into the conversation
via a per-kind registered handler, not an inline per-kind switch; new
interaction kinds SHALL be added by registering a handler. Interaction requests
SHALL be delivered to the user over the session channel, and results SHALL be
recorded.

#### Scenario: Decision recorded
- **WHEN** a user approves or denies a requested action
- **THEN** the decision and action are logged for audit

#### Scenario: Result recorded
- **WHEN** a client resolves a pending interaction (approve/deny, answers, or client-tool output)
- **THEN** the interaction kind and its result are recorded for audit

#### Scenario: Fold delegated to the kind's handler
- **WHEN** a run resumes from a resolved interaction
- **THEN** the registered handler for that interaction's kind folds the result into a tool_result, with no per-kind switch in the resume path

