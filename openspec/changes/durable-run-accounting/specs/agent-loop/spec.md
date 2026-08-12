# agent-loop Specification (delta)

## MODIFIED Requirements

### Requirement: Reactive context-overflow fallback
When the provider rejects a request as too large for the context window, the loop SHALL drop older rounds from the working view and retry a bounded number of times rather than failing the run. A `length` stop below the intended output cap SHALL be treated as the same condition: the response is discarded (never persisted), context is compacted, and the request retried. Both paths SHALL share a once-per-conversational-input recovery guard (see overflow-recovery); a second recovery within the same window fails the run.

#### Scenario: Overflow retried after dropping rounds
- **WHEN** the provider rejects a request as context-overflow
- **THEN** the loop drops the oldest round(s) from the working view and retries, up to a configured bound

#### Scenario: Non-overflow error fails the run
- **WHEN** the provider returns an error that is not context-overflow
- **THEN** the run fails with that error rather than retrying

#### Scenario: Recoverable length compacts and retries
- **WHEN** the provider returns a `length` stop below the intended output cap
- **THEN** the response is discarded, the context is compacted, and the request is retried once

#### Scenario: Second recovery within one input fails the run
- **WHEN** a second recoverable response (length or overflow-form) arrives before new conversational input was consumed
- **THEN** the run appends a give-up error entry and fails instead of compacting again
