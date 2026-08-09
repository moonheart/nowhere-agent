## ADDED Requirements

### Requirement: Provider and model administration
A platform administrator SHALL be able to create, list, update, and delete system-level providers and their models, set the platform default provider, and mark a provider's default model and vision-capable models. Provider keys SHALL be masked in listings and never returned in plaintext. Deleting a provider or model SHALL respect the deletion constraints (no deletion while assigned as a team default, platform default, or provider default).

#### Scenario: Platform administrator manages providers
- **WHEN** a platform administrator opens the providers console
- **THEN** they can create providers, edit their base URL and key, enable or disable them, and set the platform default

#### Scenario: Platform administrator manages models
- **WHEN** a platform administrator opens a provider's models
- **THEN** they can add, enable or disable, rename, mark vision-capable, override the context window, and set the default model

#### Scenario: Keys stay masked
- **WHEN** any listing of providers is returned
- **THEN** keys appear only as masked fragments

#### Scenario: Members cannot administer providers
- **WHEN** a non-platform-administrator requests the system provider administration API
- **THEN** the request is rejected

### Requirement: Team provider administration
A team owner or team administrator SHALL be able to create, list, update, and delete the team's own providers and their models. Team provider keys SHALL be encrypted at rest, masked in listings, and never returned in plaintext. System providers SHALL be visible to the team but not editable by it.

#### Scenario: Team owner manages team providers
- **WHEN** a team owner or administrator opens the team's provider management
- **THEN** they can create, edit, enable or disable, and delete the team's own providers and models

#### Scenario: Team providers stay masked
- **WHEN** a team listing of its providers is returned
- **THEN** keys appear only as masked fragments

#### Scenario: System providers are read-only for the team
- **WHEN** a team owner or administrator edits a system provider
- **THEN** the request is rejected

#### Scenario: Other teams cannot see the provider
- **WHEN** a member of another team requests a team provider's details
- **THEN** the request is rejected or the provider is not found

### Requirement: Team provider and model assignment
A team owner or team administrator SHALL be able to select the team's provider and default model — choosing among the enabled system providers and the team's own enabled providers and their models — and view the current assignment. The selection SHALL take effect on the team's next model call without a server restart. A team with no assignment SHALL use the platform default provider and model. The team SHALL NOT configure credentials beyond its own providers' keys.

#### Scenario: Team assigns a system provider and model
- **WHEN** a team owner or administrator selects a system provider and one of its models
- **THEN** the assignment is stored and used for the team's subsequent model calls

#### Scenario: Team assigns its own provider and model
- **WHEN** a team owner or administrator selects one of the team's own providers and a model
- **THEN** the assignment is stored and the team provider's key authenticates subsequent calls

#### Scenario: Team views the current assignment
- **WHEN** a team owner or administrator opens the team settings
- **THEN** the current provider and model are shown

#### Scenario: Selection constrained to enabled providers
- **WHEN** a team tries to assign a disabled provider or a disabled model
- **THEN** the assignment is rejected

#### Scenario: Members cannot change the assignment
- **WHEN** a member whose role is neither owner nor administrator changes the team assignment
- **THEN** the request is rejected

#### Scenario: No assignment uses the platform default
- **WHEN** a team has made no selection
- **THEN** the team's calls use the platform default provider and model

## REMOVED Requirements

### Requirement: Team provider credential management
**Reason**: Teams no longer supply credentials. Provider API keys are owned by the system-level provider registry, so the per-team key override and its masking/rotation UI are obsolete.
**Migration**: A team selects a provider and model from the registry instead; the provider's stored key authenticates all calls. Existing `team_api_keys` rows are ignored (the table is dropped) — operator should remove `team_api_keys` data via the migration.
