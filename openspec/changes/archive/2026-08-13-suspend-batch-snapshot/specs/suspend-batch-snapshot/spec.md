# suspend-batch-snapshot Specification (delta)

## ADDED Requirements

### Requirement: Suspended batch identity captured at suspend time
When the interaction gate suspends a tool batch, the system SHALL durably record a batch snapshot — the run ID, the session ID, and the full ordered list of the batch's tool_call IDs (gated and ungated calls alike) — in the same transaction that records the batch's first interaction row, so the suspension is bound into the durable state rather than re-derivable later.

#### Scenario: Snapshot written with the batch's interactions
- **WHEN** a run suspends on a batch of two gated calls and one ungated sibling
- **THEN** one batch snapshot row exists for that run carrying all three tool_call IDs in assistant-message block order, committed atomically with the interaction rows

#### Scenario: Snapshot insert is idempotent across frames
- **WHEN** a run suspends on several gated calls (several interrupt frames)
- **THEN** exactly one batch snapshot row exists for the run regardless of how many interaction rows accompany it

### Requirement: Fold resolves the batch from the snapshot
The fold of a suspended batch SHALL resolve the batch from its recorded snapshot: locate the last tool_use-bearing assistant message persisted under the folding run's ID, and validate that the message's tool_use ID set equals the snapshot's recorded ID set before any call is executed or folded. The system SHALL NOT infer the suspended batch from a session-wide history scan.

#### Scenario: Fold targets the suspending run's own message
- **WHEN** a session's history contains newer tool_use-bearing assistant messages from later runs and the suspended run's batch is folded
- **THEN** the fold uses the assistant message persisted under the folding run's ID, never the newer messages

#### Scenario: Snapshot mismatch fails loudly
- **WHEN** the located assistant message's tool_use IDs do not equal the snapshot's recorded ID set
- **THEN** the fold fails with an error, executes no tool call, and persists no tool_result message

#### Scenario: Missing snapshot fails loudly
- **WHEN** a fold is requested for a run with interactions but no batch snapshot
- **THEN** the fold fails with an error rather than falling back to a history heuristic

### Requirement: Fold commit is atomic and idempotent
The fold SHALL persist its folded tool_result message and mark the batch folded in one transaction. A fold requested for an already-folded batch SHALL NOT re-execute any tool call; it SHALL rebuild and return the conversation history.

#### Scenario: Resume retry does not re-execute
- **WHEN** a fold completed (tool_result message persisted, batch marked folded) and the same resume is retried
- **THEN** no tool call is executed again and the rebuilt history is returned

#### Scenario: Crash between decision and fold is recoverable
- **WHEN** every interaction of a batch is decided but the fold never committed
- **THEN** a subsequent resume attempt performs the fold and commits it

### Requirement: Submission rejected while interactions are pending
A new chat submission to a session with undecided interactions SHALL be rejected with a typed conflict error (409, `pending_interaction`), based on a durable store check, so the gate holds across gateway instances.

#### Scenario: New message blocked behind a pending approval
- **WHEN** a session has a pending approval interaction and the user submits a new chat message
- **THEN** the submission is rejected with 409 `pending_interaction` and no run is started

#### Scenario: Gate evaluates durable state across instances
- **WHEN** a pending interaction was recorded by gateway instance A and a new submission arrives at instance B
- **THEN** instance B rejects the submission, because the check reads the durable store

#### Scenario: Submission allowed after the batch resolves
- **WHEN** the session's pending interactions have all been decided and folded
- **THEN** a new chat submission is accepted normally

### Requirement: Legacy pending interactions backfilled
The migration introducing batch snapshots SHALL backfill a snapshot for every run with pending interactions from that run's last tool_use-bearing assistant message. Runs where no such message exists SHALL have their pending interactions rejected so they cannot block the submission gate forever.

#### Scenario: Pre-change approval resumes after upgrade
- **WHEN** an interaction that was pending before the migration is decided after the upgrade
- **THEN** the fold resolves the batch from the backfilled snapshot and completes normally
