## ADDED Requirements

### Requirement: Agent definition CRUD API
The platform SHALL expose an authenticated HTTP API for managing agent
definitions at three scope tiers: **self** (`/api/me/agentdefs`, any
authenticated account, own definitions), **team**
(`/api/teams/{teamID}/agentdefs`, authorized by the caller's `Role` in the
named team; platform admins satisfy it without membership), and **platform**
(`/api/admin/agentdefs`, system scope, `platform_role == admin` only).
Each tier SHALL support list, create, update, and delete. Writes SHALL carry
the full markdown document (frontmatter + body); the API SHALL validate the
document on write (parseable frontmatter, non-empty `name` and body) and store
both the parsed definition and the raw document. Delete SHALL remove only the
caller-visible definition at that scope and SHALL NOT touch built-ins.

#### Scenario: Self-tier round trip
- **WHEN** an authenticated account creates, lists, updates, and deletes a definition under `/api/me/agentdefs`
- **THEN** each operation affects only that account's user-scope definitions, and the list reflects each write immediately

#### Scenario: Team-tier authorization
- **WHEN** a team member with the required role writes a definition under `/api/teams/{teamID}/agentdefs`
- **THEN** the definition is stored at that team's scope; a member below the required role is rejected, and a non-member's response does not distinguish "team does not exist" from "not a member"

#### Scenario: Platform-tier manages system scope
- **WHEN** a platform administrator writes a definition under `/api/admin/agentdefs`
- **THEN** it is stored at system scope; a caller whose `platform_role` is `user` is rejected

#### Scenario: Invalid document rejected
- **WHEN** a write carries a document with unparseable frontmatter, an empty `name`, or an empty body
- **THEN** the write is rejected with a validation error and nothing is stored

#### Scenario: Built-ins are read-only
- **WHEN** a caller attempts to delete or overwrite the built-in `general-purpose` definition through the API
- **THEN** the request is rejected; overriding a built-in is only possible by authoring a same-named scoped definition

#### Scenario: Skills declared but unrunnable flagged on write
- **WHEN** a written definition declares `skills` while no skill script runner is available in the deployment
- **THEN** the write succeeds but the response flags the declaration as currently ineffective (same degradation the spawn path logs)
