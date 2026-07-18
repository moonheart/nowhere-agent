# model-routing Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Credential resolution
The system SHALL hold platform API keys by default. A team MAY configure its own key, which SHALL then take precedence for that team's model calls.

#### Scenario: Platform key by default
- **WHEN** a user has no team key configured
- **THEN** model calls use the platform-held key

#### Scenario: Team key override
- **WHEN** the user's team has configured its own key
- **THEN** that team's model calls use the team key

### Requirement: Routing policy
The system SHALL select provider and model per request according to a configurable routing policy.

#### Scenario: Policy-driven selection
- **WHEN** a model call is made
- **THEN** provider and model are chosen by the configured policy (not hard-coded)

### Requirement: Two-level quota and rate limiting
The system SHALL enforce quota and rate limits at two levels: platform (backstop) and team (when the team has its own key/quota).

#### Scenario: Team quota enforced
- **WHEN** a team with its own key exceeds its quota
- **THEN** further calls for that team are rejected or throttled regardless of platform quota

#### Scenario: Platform backstop
- **WHEN** platform-level quota is exceeded
- **THEN** calls are rejected or throttled even if team quota remains

### Requirement: Provider failover
On provider failure or rate-limiting, the system SHALL fail over to an alternate provider/model per policy.

#### Scenario: Failover on error
- **WHEN** the primary provider errors or is rate-limited
- **THEN** the request is retried against the configured fallback

### Requirement: Usage attribution
Every model call SHALL record token usage and cost attributed to both user and team, feeding quota enforcement.

#### Scenario: Metered call
- **WHEN** a model call completes
- **THEN** its token and cost usage is recorded against the user and their team

