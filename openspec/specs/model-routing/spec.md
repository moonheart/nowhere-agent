# model-routing Specification

## Purpose
TBD - created by archiving change init-nowhere-agent. Update Purpose after archive.
## Requirements
### Requirement: Credential resolution
> **SUPERSEDED (deprecated):** this requirement describes the deleted
> `team_api_keys` mechanism (migration 000028 drops the table). Credentials are
> now owned by provider rows in the provider registry (`provider_registry`,
> change provider-registry): the platform holds system providers and their
> keys, and a team may manage its own team-scoped providers; resolution happens
> per request via `providerreg.Resolver` (`ResolveForTeam`), which degrades to
> the platform key on any failure. Keep the archived description below for
> historical context only.

The system SHALL hold platform API keys by default. A team MAY configure its own key, which
SHALL then take precedence for that team's model calls.

Resolution SHALL happen on the request path, for the account making the call, so that a
configured team key governs that account's model calls rather than being inert configuration.
A team key SHALL only be selected when it belongs to the provider actually being called; a key
configured for a different provider SHALL be ignored. When several of the account's teams have
configured a key for that provider, selection SHALL be deterministic. Any failure to resolve a
team key SHALL fall back to the platform key rather than failing the request.

#### Scenario: Platform key by default
- **WHEN** a user has no team key configured
- **THEN** model calls use the platform-held key

#### Scenario: Team key override
- **WHEN** the user's team has configured its own key
- **THEN** that team's model calls use the team key

#### Scenario: Resolution is per request
- **WHEN** a team configures a key while the server is running
- **THEN** that team's members' subsequent model calls use it without restarting the server

#### Scenario: Provider mismatch ignored
- **WHEN** the user's team has configured a key for a provider other than the one being called
- **THEN** that key is not used and the platform key applies

#### Scenario: Resolution failure falls back
- **WHEN** resolving a team key fails
- **THEN** the request proceeds with the platform key

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
> **REMOVED (deprecated):** no failover mechanism exists in the current code.
> The agent loop retries context-overflow mid-stream (shrink + retry) but a
> failed provider/model call fails the run; there is no policy that retries a
> request against an alternate provider. The archived requirement below is
> kept for historical context only.

On provider failure or rate-limiting, the system SHALL fail over to an alternate provider/model per policy.

#### Scenario: Failover on error
- **WHEN** the primary provider errors or is rate-limited
- **THEN** the request is retried against the configured fallback

### Requirement: Usage attribution
Every model call SHALL record token usage and cost attributed to both user and team, feeding quota enforcement.

#### Scenario: Metered call
- **WHEN** a model call completes
- **THEN** its token and cost usage is recorded against the user and their team

