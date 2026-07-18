# observability Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: End-to-end run tracing
The system SHALL trace each run end to end, capturing spans for model calls, tool executions, permission checks, and sandbox operations.

#### Scenario: Step replay
- **WHEN** a completed or in-progress run is inspected
- **THEN** its steps can be replayed in order with inputs, outputs, and timings

### Requirement: LLM cost metering
Every LLM call SHALL record input/output/cached tokens and computed cost, attributed to both user and team.

#### Scenario: Per-call metering
- **WHEN** an LLM call completes
- **THEN** token counts and cost are recorded against the calling user and their team

#### Scenario: Cost feeds quota
- **WHEN** accumulated cost is queried
- **THEN** it reflects metered usage and is available to quota enforcement

### Requirement: Structured logging
Components SHALL emit structured logs with consistent correlation identifiers tying a run's activity together.

#### Scenario: Correlated logs
- **WHEN** investigating a run
- **THEN** all related log entries can be retrieved by a shared run/session identifier

### Requirement: Dreaming metrics
The dreaming worker SHALL emit metrics including episodes processed, memories written, and LLM spend versus budget.

#### Scenario: Dreaming run visibility
- **WHEN** a dreaming run completes
- **THEN** its episode count, memories written, and spend-versus-budget are recorded

