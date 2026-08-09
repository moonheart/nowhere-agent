# persist-agent-defs — design

## Context

`internal/agentdef` today is an in-memory `Store` (built-ins + a `defs` slice only
tests write). `cmd/server/main.go:506` builds it with `agentdef.NewStore()` and the
spawn tool resolves from it. The skill subsystem already solved the same problem:
`internal/skill/pgstore.go` keeps a current-pointer table (`skills`) plus an
immutable version table (`skill_versions`), and `internal/skillapi` exposes
three-tier CRUD (`me` / `teams/{id}` / `platform`) wired once in `main.go:776`
behind `RequireAuth`. The admin console has a matching skill editor under
`web/src/components/admin`. Next free migration number: **000027**.

## Goals / Non-Goals

**Goals:**
- Durable, per-scope (user/team/system) agent definitions shared across instances.
- Spawn-time resolution: PG definitions overlaid on built-ins, same priority and matching.
- Three-tier CRUD API + console page, reusing the skill patterns verbatim where possible.
- Degrade to built-ins when the DB read fails — spawns never hard-fail on a store outage.

**Non-Goals:**
- Version-history UI or rollback (the version table exists for audit; expose later if asked).
- Import/export of definition bundles.
- Per-definition usage analytics (belongs to first-class subagent runs).

## Decisions

### D1: Two-table versioning, mirroring skill exactly
`agent_defs` (id, scope fields `user_id`/`team_id` nullable, name, current_version,
created_at/updated_at, unique per (scope, name)) + `agent_def_versions` (def_id,
version, name, when_to_use, tools[], disallowed_tools[], model, max_turns, skills[],
system, raw_document, created_by, created_at). Rationale: identical lifecycle to
skills (draft→publish overwrites current, history retained), proven query patterns,
and reviewers already know the shape. Alternative considered: a single table
overwriting in place — rejected, loses audit and diverges from the platform's
established versioning convention.

### D2: Resolution via a layered store, not a rewrite of agentdef.Store
Keep `agentdef.Store` (built-ins + `Put`) and add `agentdef.PGStore` implementing
`ListVisible(ctx, scopes) ([]AgentDef, error)`. At spawn, `SpawnTool` resolves
against a merged view: built-ins ← PG defs in caller scope-priority order. The merge
moves into `agentdef` as `Resolve(ctx, name, scopes)` on a small `Resolver` that
combines both sources, so `skillToolNames`-style callers and the dynamic
`Description()` share one path. Alternative: load-all-into-memory at boot with
invalidation hooks — rejected, multi-instance deployments would serve stale defs
until restart.

### D3: Per-spawn (per-call) resolution, cached per run
Resolve once per `SpawnTool.Call` (one indexed query; definitions are few per scope),
not per model iteration. The `Description()` listing reuses the same merged view;
it tolerates store errors by listing built-ins only. Rationale: definitions change
rarely but must take effect without restart; the query cost is trivial next to a
child agent run.

### D4: agentdefapi mirrors skillapi's handler shape
`internal/agentdefapi` with `NewHandler(identitySvc, store).RegisterAuthed(mux, RequireAuth)`
and three route families:
- `GET/POST /api/me/agentdefs`, `PUT/DELETE /api/me/agentdefs/{name}`
- `GET/POST /api/teams/{teamID}/agentdefs`, `PUT/DELETE /api/teams/{teamID}/agentdefs/{name}`
- `GET/POST /api/admin/agentdefs`, `PUT/DELETE /api/admin/agentdefs/{name}`

Authorization reuses the existing three-tier helpers (`caller(r)`, team-role check,
platform admin check). Wire format: `{document: "<markdown>"}` on write; reads return
parsed fields plus the raw document. Validation lives in `agentdef` (shared by API
and any future importer): frontmatter must parse, `name` and body non-empty; a
`warnings` array on the response flags declared-but-unrunnable `skills`.

### D5: Console page follows the skill editor's layout
`web/src/lib/agentdefs.ts` typed client + `AgentDefsPage` under
`web/src/components/admin`, registered in the admin router next to skills. Sections
render per authorized tier (self always; teams by role; system for platform admins),
hidden tiers are omitted. Editor is a plain markdown textarea with a frontmatter
cheat-sheet (the skill editor's richer resource/script tooling does not apply).

## Risks / Trade-offs

- [PG read outage at spawn time] → Resolver catches store errors, logs, resolves built-ins only (spec scenario "Store unavailable degrades to built-ins").
- [A team/system def shadows a built-in unexpectedly for some users] → Resolution is explicit scope priority (unchanged semantics); the console list shows which scope each visible def comes from in a follow-up if needed.
- [Two-table write skew (pointer update without version row)] → Single transaction per save, same as skill store's `Put`.
- [Migration ordering vs. concurrent deploys] → golang-migrate's serial application; down migration drops both tables.

## Migration Plan

1. Apply migration `000027_agent_defs` (up: create both tables + indexes; down: drop).
2. Deploy server (PG store wired; API routes registered; spawn resolves through layered store).
3. Deploy web (console page). Rollback: revert binaries; run down migration only if no authored defs must be preserved.

## Open Questions

- Exact minimum team role for team-tier writes: mirror skills (same helper) rather than inventing a new threshold — confirm during implementation against `skillapi/teams.go`.
