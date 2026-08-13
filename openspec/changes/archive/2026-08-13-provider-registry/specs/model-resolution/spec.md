## ADDED Requirements

### Requirement: Per-request provider and model resolution
The system SHALL resolve provider and model per request, for the account making the call, from the Postgres registry rather than environment variables. Resolution SHALL prefer the account's team assignment (the team's selected provider and default model), where that provider MAY be a system provider or the team's own provider; with no assignment it SHALL use the platform default provider and that provider's default model. Resolution SHALL happen on the request path, so a change to the registry or a team assignment takes effect on the next request without a restart.

#### Scenario: Team assignment over a system provider
- **WHEN** the user's team has selected a system provider and one of its models
- **THEN** that provider and model are used for the user's model calls

#### Scenario: Team assignment over a team provider
- **WHEN** the user's team has selected one of its own providers and a model
- **THEN** that team provider's key and model are used

#### Scenario: Platform default fallback
- **WHEN** the user's team has no assignment
- **THEN** the platform default provider and its default model are used

#### Scenario: Resolution reflects live configuration
- **WHEN** a team changes its model while the server is running
- **THEN** subsequent calls use the new model without a restart

#### Scenario: No provider configured
- **WHEN** no enabled provider exists in the registry
- **THEN** chat is disabled and no model call is attempted

### Requirement: Provider credential usage
The system SHALL use the resolved provider's own stored API key for model calls — the system provider's key when a system provider is assigned, the team provider's key when a team-owned provider is assigned. There SHALL be no per-team credential override: a team selects provider and model, and the credential always comes from the provider registry row.

#### Scenario: System provider key used
- **WHEN** a request resolves to a system provider
- **THEN** the system provider's stored key authenticates the call

#### Scenario: Team provider key used
- **WHEN** a request resolves to a team-owned provider
- **THEN** that team provider's stored key authenticates the call

#### Scenario: No team key path
- **WHEN** a team changes its provider or model
- **THEN** credentials come from the selected provider and are unchanged by the selection

### Requirement: Vision model resolution
The `view_image` tool SHALL resolve its model from the same registry: the vision-capable model of the resolved (team-assigned, which may be a team-owned or system provider, else platform-default) provider, preferring the provider's default vision model when one is marked. When the resolved provider has no vision-capable model, the tool SHALL be unavailable for that run.

#### Scenario: Assigned provider has a vision model
- **WHEN** the resolved provider — system or team-owned — has a vision-capable model
- **THEN** the `view_image` tool uses that model

#### Scenario: Team provider vision model
- **WHEN** the team's own provider has a vision-capable model and is assigned
- **THEN** the `view_image` tool uses the team provider's vision model

#### Scenario: No vision model available
- **WHEN** the resolved provider has no vision-capable model
- **THEN** the `view_image` tool is not registered for the run

### Requirement: Model reference resolution
A run that carries an explicit model reference (a scheduled task or an agent definition) SHALL resolve that reference against the resolved provider's enabled models by name. An empty reference SHALL fall back to the team default, then the platform default. A reference that names no enabled model on the resolved provider SHALL fail the run with a clear error rather than silently substituting a different model.

#### Scenario: Reference matches an enabled model
- **WHEN** a task names a model that exists and is enabled on the resolved provider
- **THEN** the run uses that model

#### Scenario: Empty reference falls back
- **WHEN** a task carries no model
- **THEN** the team default, or the platform default, is used

#### Scenario: Unknown reference fails closed
- **WHEN** a task names a model that is not enabled on the resolved provider
- **THEN** the run reports an error and does not execute against a substituted model

### Requirement: Capability profile integration
The resolved provider+model pair SHALL feed the existing capability lookup (context window, `ImageInput`) by vendor and model name. A model-row context-window override SHALL take precedence over the capability table.

#### Scenario: Known model keeps its profile
- **WHEN** a resolved model is in the capability table
- **THEN** its context window and image-input capability are used as today

#### Scenario: Override wins
- **WHEN** a resolved model has a registry context-window override
- **THEN** the override is used for compression
