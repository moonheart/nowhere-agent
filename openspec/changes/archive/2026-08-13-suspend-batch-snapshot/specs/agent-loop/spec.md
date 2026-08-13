# agent-loop Specification (delta)

## MODIFIED Requirements

### Requirement: General interrupt primitive
The loop SHALL treat any tool call that needs client interaction as a single
general interrupt, not as per-kind special cases. A tool call SHALL suspend the
run when it is (a) gated for approval, (b) the built-in `ask_user` question tool,
or (c) a client-side tool. On any interrupt the loop SHALL emit one unified
interrupt frame per gated call, record a pending interaction, and end the run
cleanly, leaving the interrupting assistant `tool_use` persisted for a later
resume. Each interrupt frame SHALL carry the full ordered batch of tool calls
(gated and ungated siblings alike) so the run worker can durably record the
suspended batch snapshot together with the interaction rows.

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

#### Scenario: Interrupt frame carries the full batch
- **WHEN** a run suspends on a batch containing both gated and ungated calls
- **THEN** the emitted interrupt frames carry every call of the batch in assistant-message block order, not just the gated ones
