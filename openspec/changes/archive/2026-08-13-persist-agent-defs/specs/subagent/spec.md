## MODIFIED Requirements

### Requirement: Agent definitions
Agent types SHALL be defined as markdown documents at system, team, and user
scopes, with frontmatter (`name`, `description`, optional `tools`,
`disallowedTools`, `model`, `maxTurns`, `skills`) and a body used as the child's
system prompt. Authored definitions SHALL be stored durably (Postgres) so they
survive restarts and are shared across instances; the parsed definition and the
raw document SHALL both be retained. Definitions SHALL merge across scopes with
user > team > system priority, and authored definitions SHALL override the
built-ins. A built-in `general-purpose` definition SHALL always be available.
Type resolution SHALL match exactly first, then by a normalized form
(case-insensitive, ignoring spaces, dashes, and underscores). A deployment with
no database or an unreachable definitions table SHALL degrade to built-ins only
rather than failing spawns.

#### Scenario: Definition parsed from markdown
- **WHEN** an agent markdown document is loaded
- **THEN** its frontmatter populates the definition and its body becomes the child's system prompt

#### Scenario: Authored definition persists across restarts
- **WHEN** a definition is created through the management API and the server restarts
- **THEN** the definition still resolves for subsequent spawns at its scope

#### Scenario: Scope override
- **WHEN** an agent type is defined at multiple scopes
- **THEN** the higher-priority scope's definition is used (user overrides team overrides system)

#### Scenario: Authored definition overrides built-in
- **WHEN** a scoped definition shares its name with a built-in definition
- **THEN** the scoped definition wins for callers in that scope, and the built-in remains for other scopes

#### Scenario: Built-in default present
- **WHEN** no user/team/system agent definitions exist
- **THEN** the `general-purpose` type is still resolvable and spawnable

#### Scenario: Normalized type match
- **WHEN** a requested type differs from a defined type only by case or separators
- **THEN** it resolves to that definition; a genuinely ambiguous request errors with the candidate list

#### Scenario: Store unavailable degrades to built-ins
- **WHEN** the durable definition store cannot be read at spawn time
- **THEN** resolution falls back to the built-in definitions and the degradation is logged, not surfaced as a spawn failure
