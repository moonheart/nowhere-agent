# session-runtime Specification (delta)

## MODIFIED Requirements

### Requirement: Single active run and multi-writer prevention
A session SHALL run at most one run at a time. State changes SHALL be synchronized to all attached clients, and clients SHALL be blocked from starting a new run while one is active. A session with undecided interactions SHALL also reject new run submissions, so a suspended batch can never be buried under newer turns; the rejection SHALL be based on the durable store so it holds across gateway instances.

#### Scenario: State synced across clients
- **WHEN** one client starts a run
- **THEN** all other attached clients are synced to the running state

#### Scenario: Blocked concurrent submission
- **WHEN** a run is active and another client submits a new run
- **THEN** the new submission is rejected or queued until the active run completes

#### Scenario: Blocked submission behind pending interactions
- **WHEN** no run is active but the session has undecided interactions, and a client submits a new run
- **THEN** the submission is rejected with a typed pending-interaction conflict and no run is started
