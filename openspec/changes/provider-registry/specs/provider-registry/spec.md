## ADDED Requirements

### Requirement: Provider registry
The system SHALL maintain a Postgres registry of providers in two scopes: **system** (visible to every team, managed by platform administrators) and **team** (owned by one team, visible only to that team's members, managed by the team's owners and administrators). Each provider SHALL have a unique name within its scope, a vendor (`anthropic`|`openai`), an optional base URL override, an API key, an enabled flag, and a default-model reference. A system provider SHALL be selectable as the platform default. Only a platform administrator SHALL create, update, or delete system providers; only a team owner or administrator SHALL create, update, or delete that team's providers. Provider API keys SHALL be encrypted at rest.

#### Scenario: Platform administrator creates a system provider
- **WHEN** a platform administrator creates a provider with a vendor, base URL, and API key
- **THEN** the provider is stored with an encrypted key, is visible to every team, and appears in the registry

#### Scenario: Team owner creates a team provider
- **WHEN** a team owner or administrator creates a provider scoped to their team
- **THEN** the provider is stored with an encrypted key and is visible only to that team's members

#### Scenario: Keys are never returned in plaintext
- **WHEN** the registry is listed through any API
- **THEN** no response field contains a stored key; only a masked fragment identifies it

#### Scenario: Multiple providers coexist
- **WHEN** two providers of the same or different vendors are configured
- **THEN** both are available for selection and neither overrides the other

#### Scenario: Team provider is not visible to other teams
- **WHEN** a member of team B lists providers
- **THEN** team A's team-scoped providers do not appear

#### Scenario: Provider names are scoped
- **WHEN** two teams each create a provider named `openai`
- **THEN** both coexist without conflict, and neither collides with a system provider of the same name

### Requirement: Model registry
Each provider SHALL expose one or more models. A model SHALL have a name (the provider API model identifier), a display name, an enabled flag, and MAY declare a context-window override and a vision-capable flag. Each provider SHALL have at most one model marked as its default. Only a platform administrator SHALL create, update, or delete system providers' models; only a team owner or administrator SHALL manage that team's providers' models. Deleting a provider SHALL delete its models.

#### Scenario: Multiple models under one provider
- **WHEN** a platform administrator adds two models to one provider
- **THEN** both are available under that provider and either may be selected

#### Scenario: Team provider models
- **WHEN** a team owner or administrator adds a model to a team provider
- **THEN** the model is available under that team provider for the team's selection

#### Scenario: Vision-capable model flagged
- **WHEN** a model can accept image input
- **THEN** it SHALL be marked vision-capable so the `view_image` tool can use it

#### Scenario: Context window override
- **WHEN** a model's context window is not derivable from the capability table
- **THEN** the model row may override it and compression uses the override

### Requirement: Deletion constraints
The system SHALL prevent deletion that would orphan a live reference. A provider SHALL NOT be deleted while any team assigns to it. A system provider SHALL NOT be deleted while it is the platform default. A model SHALL NOT be deleted while it is a team's default model or the provider's default model.

#### Scenario: Provider in use is protected
- **WHEN** an administrator tries to delete a provider that a team assigns to
- **THEN** the deletion is rejected with a clear error and the provider remains

#### Scenario: Default provider is protected
- **WHEN** a platform administrator tries to delete the platform default provider
- **THEN** the deletion is rejected unless a replacement default is chosen first

#### Scenario: Team provider protected within scope
- **WHEN** a team owner tries to delete a team provider that the team's assignment uses
- **THEN** the deletion is rejected with a clear error

### Requirement: Encryption of provider keys
Provider API keys SHALL be encrypted at rest with the same encryptor used for team keys today, and decrypted only on the resolution path. Enabling encryption later SHALL be a gradual migration, not a flag day.

#### Scenario: Encrypted at rest
- **WHEN** a provider key is stored
- **THEN** the stored value is an encrypted envelope, not the plaintext key

#### Scenario: Decrypt failure degrades
- **WHEN** a stored provider key fails to decrypt on the resolution path
- **THEN** the failure is surfaced loudly and the call fails rather than sending a corrupt credential
