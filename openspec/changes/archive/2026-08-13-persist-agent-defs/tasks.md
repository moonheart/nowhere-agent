## 1. Storage layer

- [x] 1.1 Write migration `migrations/000027_agent_defs.up.sql` / `.down.sql` (agent_defs pointer table + agent_def_versions history table, unique (user_id, team_id, name) scoping, indexes); verify `go run ./cmd/migrate` applies cleanly both ways
- [x] 1.2 Add `agentdef.PGStore` (`internal/agentdef/pgstore.go`): `ListVisible(ctx, scopes)`, `Get`, `Put` (transactional pointer+version save), `Delete`, mirroring `internal/skill/pgstore.go` query patterns
- [x] 1.3 Add `agentdef` document validation shared by API/importers: parse frontmatter, require non-empty name/body, return structured warnings (e.g. skills declared without runner)
- [x] 1.4 PG store tests against dev Postgres (unique random names, delete only created rows by ID): round trip per scope, version increments, delete isolation

## 2. Resolution

- [x] 2.1 Add `agentdef.Resolver`: merged view built-ins ← PG defs in caller scope-priority order; `Resolve(ctx, name, scopes)` and `Available(ctx, scopes)`; store errors degrade to built-ins with a slog warning
- [x] 2.2 Rewire `SpawnTool` to resolve through the Resolver (store field becomes the merged view source; keep normalized/exact matching and error-message candidate lists identical)
- [x] 2.3 Update `SpawnTool.Description()` to list types from the Resolver's merged view
- [x] 2.4 Resolver unit tests: scope override, built-in shadowing, unavailable store degrades to built-ins, normalized match across PG defs

## 3. HTTP API

- [x] 3.1 Create `internal/agentdefapi` handler with three route families (`/api/me/agentdefs`, `/api/teams/{teamID}/agentdefs`, `/api/admin/agentdefs`; GET/POST + PUT/DELETE by name), reusing skillapi's caller/team-role/platform-admin helpers
- [x] 3.2 Wire-format: `{document}` on write, parsed fields + raw document on read; validation errors return 400 with details; skills-without-runner flagged in response `warnings`
- [x] 3.3 Register routes in `cmd/server/main.go` behind `RequireAuth`, replacing `agentdef.NewStore()` wiring with the PG-layered resolver (built-ins remain the fallback)
- [x] 3.4 API tests (real dev Postgres, unique names): self-tier round trip, team-tier role enforcement + non-member opacity, platform-tier admin-only, invalid document rejected, built-in delete/overwrite rejected

## 4. Console page

- [x] 4.1 Add `web/src/lib/agentdefs.ts` typed client for the three tiers
- [x] 4.2 Add `AgentDefsPage` under `web/src/components/admin` (list with name/scope/when-to-use/model+tool overrides, markdown editor with frontmatter cheat-sheet, delete with confirmation, validation errors surfaced in-place)
- [x] 4.3 Register the page in the admin router/nav next to skills; hide unauthorized tiers
- [x] 4.4 `pnpm exec tsc -b` and `pnpm lint` clean; `pnpm build` succeeds

## 5. Verification

- [x] 5.1 `go build ./... && go vet ./... && go test ./...` green
- [x] 5.2 `openspec validate persist-agent-defs` passes
- [ ] 5.3 Manual smoke: create a user-scope def via console, spawn it in chat, restart server, spawn again (persistence), delete it (falls back to built-ins)
