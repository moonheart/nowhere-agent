# admin-console — delta for memory-consolidation

## ADDED Requirements

### Requirement: Self-service consolidation
The console SHALL let an authenticated account trigger consolidation of its own
long-term memory, and SHALL report whether a pass is running and how that
account's last triggered pass went.

The route SHALL take the account from the authenticated request rather than from
a parameter, so there is no input through which one account could aim a pass at
another's sessions.

When the deployment has no consolidation worker — a deployment with no model
provider configured has none — the console SHALL degrade to hiding the control
rather than failing the view around it.

#### Scenario: Triggering from the console
- **WHEN** an authenticated account asks the console to consolidate
- **THEN** a pass over that account's own sessions begins and the request is answered without waiting for it

#### Scenario: Trigger while a pass is running
- **WHEN** an account asks to consolidate while a pass is already in flight
- **THEN** the request is refused as a conflict and no second pass starts

#### Scenario: Reporting the last pass
- **WHEN** an account's triggered pass has finished
- **THEN** the console reports what it changed — memories added, revised, retired — or the failure if it failed

#### Scenario: One account's pass is not visible to another
- **WHEN** an account has never triggered a pass
- **THEN** it is shown no pass history, including that of other accounts

#### Scenario: No worker configured
- **WHEN** the deployment has no consolidation worker
- **THEN** the consolidation control is absent and the rest of the memory view still works
