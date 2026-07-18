# identity-scope Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: User accounts and authentication
The system SHALL provide user accounts with authentication for the multi-user platform.

#### Scenario: Authenticated access
- **WHEN** a user authenticates with valid credentials
- **THEN** a session/token is issued and subsequent requests are attributed to that user

### Requirement: Teams
The system SHALL support teams as a grouping of users for shared resources.

#### Scenario: Team membership
- **WHEN** a user is added to a team
- **THEN** they gain access to that team's shared skills and memories

### Requirement: Shared scope model
The system SHALL define a single scope model (user/team/system) reused by skills and memory for ownership, isolation, and access control.

#### Scenario: Consistent scoping
- **WHEN** a resource (skill or memory) is created
- **THEN** it is tagged with exactly one of user, team, or system scope using the shared model

#### Scenario: Scope-based access
- **WHEN** a resource is accessed
- **THEN** the shared scope model determines visibility: user-private, team-shared, or system-global

