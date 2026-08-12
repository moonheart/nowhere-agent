# overflow-recovery Specification

## Purpose
Classification and bounded recovery for context-overflow conditions: a `length` stop whose output is below the intended cap is treated as recoverable (discarded, compacted, retried once per conversational input), with a persisted guard that survives restarts.

## ADDED Requirements
### Requirement: Recoverable-length classification
When an assistant response stops with `length`, the run SHALL classify it as recoverable iff the actual output usage is below the intended output cap — the caller-supplied `maxTokens` when set, else the model's `maxTokens` — as measured before any context clamping. A stop that reached the intended cap SHALL be treated as a genuine output-limit stop (normal completion). The classification SHALL NOT use context-percentage heuristics.

#### Scenario: Truncated below the intended cap
- **WHEN** a response stops with `length` and its output usage is below the intended output cap
- **THEN** the response is classified recoverable

#### Scenario: Cap fully used
- **WHEN** a response stops with `length` and its output usage reached the intended cap
- **THEN** the response is classified as a genuine output-limit stop and completes normally

### Requirement: Recoverable response discarded and retried once
A recoverable response SHALL be discarded: it SHALL NOT be persisted as a message, SHALL NOT enter the working view, and its provisioned result id stays unfulfilled. Its usage SHALL already be durable in the ledger (see usage-ledger). The run SHALL then compact the context and retry the step once per conversational input.

#### Scenario: Discarded response never persisted
- **WHEN** a response is classified recoverable
- **THEN** no message row and no content frame for it is persisted; the run compacts and retries

#### Scenario: Cost preserved
- **WHEN** a recoverable response is discarded
- **THEN** its usage is present in `usage_records` before the discard

### Requirement: Once-per-input recovery guard
The run SHALL allow at most one overflow recovery (length-classified or overflow-form error) per consumed conversational input — the run's prompt, steering, or follow-up. A second recoverable response within the same window SHALL append a give-up error entry and fail the run. The guard SHALL be persisted (derivable from step records) so it survives a restart. Overflow-form provider errors SHALL share the same guard and take the same path.

#### Scenario: Second recovery fails the run
- **WHEN** a second recoverable response arrives before any new conversational input was consumed
- **THEN** the run appends a give-up error entry and fails, rather than compacting again

#### Scenario: Guard persists across restart
- **WHEN** a run recovers once, crashes, and is resumed/recovered
- **THEN** the persisted guard still counts the first recovery; a second recoverable response still fails the run

#### Scenario: New input resets the window
- **WHEN** new conversational input is consumed after an overflow recovery
- **THEN** the guard resets and one further recovery is allowed

### Requirement: User-facing wording for truncation
When a run fails or ends after a truncated response, user-facing wording SHALL state neutrally that the response was truncated before completion; it SHALL NOT claim the configured output limit was reached.

#### Scenario: Neutral truncation message
- **WHEN** a run ends after a length-truncated response
- **THEN** the user-facing message says the response was truncated before completion, without claiming the output limit was reached
