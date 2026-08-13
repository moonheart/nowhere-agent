## MODIFIED Requirements

### Requirement: Credential resolution
The system SHALL authenticate model calls with the resolved provider's API key, stored and encrypted in the provider registry. A team selects a provider and model but does not supply credentials. A provider key that fails to decrypt SHALL fail the call loudly rather than fall back to a different key.

Resolution SHALL happen on the request path, for the account making the call, so that a change to the registry or a team assignment takes effect without a restart. Any failure to resolve a provider SHALL disable chat rather than route to an unconfigured vendor.

#### Scenario: Provider key authenticates
- **WHEN** a model call resolves to a provider
- **THEN** the provider's stored key is used

#### Scenario: No per-team key override
- **WHEN** the user's team has configured its own provider key in a legacy `team_api_keys` row
- **THEN** the legacy row is ignored and the provider's key is used

#### Scenario: Resolution reflects live configuration
- **WHEN** a provider key is rotated in the registry while the server is running
- **THEN** subsequent calls use the rotated key without a restart

#### Scenario: Decrypt failure fails loudly
- **WHEN** a provider's stored key cannot be decrypted
- **THEN** the call fails with an error rather than proceeding with a corrupt or substitute credential

### Requirement: Routing policy
The system SHALL select provider and model per request according to a DB-driven routing policy: the account's team assignment when present, otherwise the platform default provider and its default model. Provider and model SHALL NOT come from environment variables or hard-coded defaults.

#### Scenario: Policy-driven selection
- **WHEN** a model call is made
- **THEN** provider and model are chosen by the team assignment or the platform default from the registry

#### Scenario: Team assignment wins
- **WHEN** the user's team has selected a provider and model
- **THEN** that selection is used even when the platform default differs

#### Scenario: No provider configured
- **WHEN** no enabled provider exists in the registry
- **THEN** no model call is made and chat is disabled
