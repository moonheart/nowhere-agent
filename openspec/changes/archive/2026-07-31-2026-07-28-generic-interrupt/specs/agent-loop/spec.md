# agent-loop — spec delta (generic-interrupt)

## ADDED Requirements

### Requirement: General interrupt primitive
The loop SHALL treat any tool call that needs client interaction as a single
general interrupt, not as per-kind special cases. A tool call SHALL suspend the
run when it is (a) gated for approval, (b) the built-in `ask_user` question tool,
or (c) a client-side tool. On any interrupt the loop SHALL emit one unified
interrupt frame, record a pending interaction, and end the run cleanly, leaving
the interrupting assistant `tool_use` persisted for a later resume.

#### Scenario: Approval suspends via the interrupt
- **WHEN** a tool call is gated for approval
- **THEN** the loop emits an interrupt frame of kind `tool_approval`, records a pending interaction, and ends the run without executing the call

#### Scenario: ask_user suspends via the same interrupt
- **WHEN** the model calls `ask_user`
- **THEN** the loop emits an interrupt frame of kind `ask_user` through the same interrupt path as approval, not a separate branch

#### Scenario: Client-side tool suspends via the interrupt
- **WHEN** the model calls a tool marked client-side
- **THEN** the loop emits an interrupt frame of kind `client_tool` and ends the run without executing the call server-side

#### Scenario: No per-kind special-casing
- **WHEN** a new interaction kind is introduced
- **THEN** it suspends through the same interrupt check and resume path without editing the loop body

### Requirement: Client-side tool contract
A tool SHALL be able to declare that it executes in the client rather than on
the server via an optional interface that leaves the base `Tool` contract
untouched. A client-side tool SHALL NOT be executed by the server; the loop
SHALL suspend on it and hand the call to the client.

#### Scenario: Client-side detection
- **WHEN** a tool implements the client-side optional interface returning true
- **THEN** the loop treats a call to it as an interrupt rather than dispatching it

#### Scenario: Base contract unchanged
- **WHEN** a tool does not implement the client-side interface
- **THEN** it is dispatched normally with no change to the `Tool` interface

### Requirement: Client-tool result is validated before folding
Client-returned output for a client-side tool SHALL be validated against the
tool's declared output schema before being folded into the conversation. Valid
output SHALL become the `tool_result`; invalid output or a client-reported error
SHALL become an `is_error` `tool_result` so the model can self-correct.

#### Scenario: Valid client output folded
- **WHEN** the client returns output conforming to the tool's declared output schema
- **THEN** it is folded as the tool_result on resume

#### Scenario: Invalid client output rejected
- **WHEN** the client returns output violating the declared output schema
- **THEN** it is folded as an is_error tool_result describing the mismatch, not trusted blindly

#### Scenario: Client-reported error folded as error
- **WHEN** the client returns an error for a client-side tool call
- **THEN** it is folded as an is_error tool_result
