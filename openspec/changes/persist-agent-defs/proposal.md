# persist-agent-defs

## Why

Agent definitions (`agentdef`) live only in a process-local in-memory store: `Store.Put` has no production caller, so the three-tier scope model (user > team > system) is unreachable at runtime and every definition vanishes on restart. On a multi-tenant platform, users and teams cannot author, persist, or manage their own subagent types — the spawn tool can only ever resolve the single built-in `general-purpose` definition.

## What Changes

- Add a Postgres-backed agent definition store (new migration + `agentdef.PGStore`) holding versioned definitions per scope (user/team/system), mirroring the skill store's two-table versioning model.
- Resolve definitions at spawn time from PG overlaid on the in-memory built-ins, preserving the existing user > team > system > built-in priority and normalized matching.
- Add an authenticated CRUD HTTP API (`internal/agentdefapi`) aligned with `skillapi`'s three-tier authorization (self / team / platform) so users author their own definitions, team leads manage team definitions, and platform admins manage system definitions.
- Add an agent-definitions management page to the admin console (list, create, edit, delete; scope-aware).
- Definition documents keep the existing markdown format (frontmatter `name`/`description`/`tools`/`disallowedTools`/`model`/`maxTurns`/`skills` + body as system prompt); storage is the parsed form plus the raw document.
- Definition validation on write: reject empty names/bodies, unknown tool names are tolerated but declared `skills` are checked against the same runner-availability rule the spawn path warns about.

## Capabilities

### New Capabilities
- `agentdef-api`: Authenticated CRUD for agent definitions across user/team/system scopes, with three-tier authorization (self/team/platform) matching the management-console model, and write-time validation.

### Modified Capabilities
- `subagent`: The "Agent definitions" requirement changes — definitions are sourced from a durable per-scope store (PG) overlaid on built-ins, not process-local memory; resolution keeps the same priority and matching rules.
- `admin-console`: The console gains an agent-definitions management surface (list/create/edit/delete per scope) under the same authorization tiers as skills.

## Impact

- **New code**: `internal/agentdef` PG store + migration (`migrations/0000xx_agent_defs`), `internal/agentdefapi` HTTP handlers, `web/src/lib/agentdefs.ts` typed client, `web/src/components/admin` agent-definitions page.
- **Wiring**: `cmd/server/main.go` constructs the PG store and registers the API routes; the spawn tool resolves through the PG-overlaid store.
- **DB**: one new migration; no changes to existing tables.
- **No breaking changes**: the built-in `general-purpose` definition and in-memory `Store` semantics remain as the fallback layer; existing spawn behavior is unchanged when no authored definitions exist.
