# execution-permission — spec delta (generic-interrupt)

## MODIFIED Requirements

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
