# session-runtime Specification (delta)

## ADDED Requirements

### Requirement: Startup recovery inspects step intents
Startup recovery for non-terminal runs SHALL inspect the run's durable step intents (see run-step-intents) before settling it: a run whose latest step intent has no result message is an interrupted step; a run whose latest step completed has the message as its authoritative outcome. Recovery SHALL NOT mark every non-terminal run failed without this inspection, and SHALL surface the persisted attempt count for interrupted steps.

#### Scenario: Interrupted run identified at startup
- **WHEN** the process starts and a run is non-terminal with an intent whose result message is missing
- **THEN** recovery records the interrupted step and its persisted attempt count instead of a blind failure

#### Scenario: Completed step run identified at startup
- **WHEN** the process starts and a run is non-terminal with its latest step result present
- **THEN** recovery reports the step completed, with the message as the authoritative outcome

#### Scenario: No intents preserves behavior
- **WHEN** the process starts and a non-terminal run has no step intents at all
- **THEN** recovery behaves as before the change (no interrupted step to report)
