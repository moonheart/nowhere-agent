# run-step-intents Specification

## Purpose
Durable step intent records for runs: before an effect (assistant step, tool invocation) the run writes a `run_steps` row with a pre-provisioned result message id and a durable attempt count, so a crash leaves a decidable boundary ("intent without result") and retry counts survive process restarts.

## ADDED Requirements
### Requirement: Intent written before the effect
For each assistant step and each tool invocation, the run SHALL write a `run_steps` row before the effect starts. The row SHALL carry the step kind (`assistant` | `tool`), the attempt number within the step, and the pre-provisioned id of the message the effect is expected to produce. The result message, once appended, SHALL carry exactly that provisioned id.

#### Scenario: Assistant intent precedes the request
- **WHEN** a run is about to make an assistant provider request
- **THEN** a `run_steps` row with kind `assistant`, the current attempt count, and a provisioned result message id exists before the request starts

#### Scenario: Tool intent precedes execution
- **WHEN** a run is about to execute a tool call
- **THEN** a `run_steps` row with kind `tool`, the tool call identity, and a provisioned result message id exists before the tool executes

#### Scenario: Result uses the provisioned id
- **WHEN** the effect completes and its result message is persisted
- **THEN** the message row uses the id pre-provisioned in the intent record

### Requirement: Durable attempt counts
The attempt number in `run_steps` SHALL be the durable retry count for the step. Retries of a step SHALL increment the count in the intent record; a process restart SHALL NOT reset it. A crash-restart loop therefore cannot exceed the configured attempt bound across restarts.

#### Scenario: Count survives restart
- **WHEN** a run retries a step twice, crashes, and the retry policy is applied again
- **THEN** the next attempt starts from the persisted count, not from zero

### Requirement: Crash-site decidable recovery
At startup recovery, a run whose latest step intent has no result message SHALL be distinguished from one whose latest step completed. Recovery SHALL inspect `run_steps` (newest first) before settling a non-terminal run: an intent without a result marks an interrupted step; a result with the provisioned id marks a completed step. Recovery SHALL record which case applies (log/run metadata) and SHALL NOT blindly fail runs without inspection.

#### Scenario: Interrupted step identified
- **WHEN** a run crashed after writing an intent but before persisting its result message
- **THEN** recovery identifies the step as interrupted and surfaces the persisted attempt count

#### Scenario: Completed step identified
- **WHEN** a run crashed after persisting its result message
- **THEN** recovery identifies the step as completed; the message remains the authoritative outcome

#### Scenario: No intents yet
- **WHEN** a run crashed before writing any step intent
- **THEN** recovery reports the run as having no interrupted step, matching pre-change behavior
