# Spec: skill-system

## ADDED Requirements

### Requirement: General-form skills
A skill SHALL be a package comprising a SKILL.md (instructions/process) plus optional resources and executable scripts.

#### Scenario: Skill composition
- **WHEN** a skill is defined
- **THEN** it contains a SKILL.md and may bundle resources and scripts

### Requirement: Progressive disclosure
Skills SHALL load in three levels to conserve context: L0 (name + one-line description, always resident), L1 (full SKILL.md body when selected), L2 (referenced resources/scripts on demand).

#### Scenario: L0 resident metadata
- **WHEN** a session begins
- **THEN** only skill names and short descriptions are present in context

#### Scenario: L1 on selection
- **WHEN** the agent selects a skill
- **THEN** its full SKILL.md body is loaded into context

#### Scenario: L2 on demand
- **WHEN** the SKILL.md references a resource or script that is needed
- **THEN** that item is loaded only at that point

### Requirement: Sandboxed script execution
Skill scripts (L2) SHALL execute inside the session's sandbox via SandboxPort.

#### Scenario: Script runs in sandbox
- **WHEN** a skill script is invoked
- **THEN** it runs in the session's isolated sandbox, not on the host

### Requirement: Three-level scope
Skills SHALL exist at system, team, and user scopes, merged at load time with priority override (user > team > system).

#### Scenario: Scope merge and override
- **WHEN** skills with the same name exist at multiple scopes
- **THEN** the higher-priority scope's version is used (user overrides team overrides system)

#### Scenario: Team sharing
- **WHEN** a skill is team-scoped
- **THEN** it is available to members of that team and not to other users

### Requirement: Skill versioning
Skills SHALL be versioned. A scope override SHALL record which version it overrides, and upstream updates SHALL surface as reviewable changes rather than silently breaking overrides. Version history SHALL support rollback.

#### Scenario: Override pinned to a version
- **WHEN** a user-scoped skill overrides a team/system skill
- **THEN** the override records the version it was based on

#### Scenario: Upstream update is reviewable
- **WHEN** an overridden team/system skill is updated
- **THEN** existing overrides are flagged for review instead of breaking silently

#### Scenario: Rollback
- **WHEN** a skill version proves faulty
- **THEN** it can be rolled back to a prior version
