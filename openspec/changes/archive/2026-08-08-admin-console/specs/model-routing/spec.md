# model-routing — delta for admin-console

## MODIFIED Requirements

### Requirement: Credential resolution
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
