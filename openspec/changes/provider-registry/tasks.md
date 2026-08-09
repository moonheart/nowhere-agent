# Tasks for provider-registry

## 1. Schema

- [x] 1.1 Add migration `000028_provider_registry.up.sql` / `.down.sql`: `providers` (scope `system`|`team` + `team_id`), `provider_models`, `team_provider_settings` tables; partial unique indexes for scoped names and single defaults (system provider `is_default`, per-provider model `is_default`)
- [x] 1.2 Extend the same migration to `DROP TABLE IF EXISTS team_api_keys` (000003)
- [x] 1.3 Add `-seed-from-env` flag to `cmd/migrate` that imports `LLM_*`/`VISION_*` into system providers/models only when the `providers` table is empty

## 2. providerreg package (store)

- [x] 2.1 Add `internal/providerreg/store.go`: `Provider` (with `Scope`/`TeamID`), `Model`, `TeamAssignment` types + `Store` interface (providers/models CRUD, `SetDefaultProvider`, `SetDefaultModel`, `TeamAssignment`/`SetTeamAssignment`/`ClearTeamAssignment`, `PlatformDefault`, `ModelsFor`, `VisibleToTeam`/`VisibleToUser`)
- [x] 2.2 Add `internal/providerreg/pgstore.go`: PG implementation with encrypted keys via `secrets.Encryptor` (gradual-migration semantics), masked key reads, scoped-name and default-conflict errors, team-visibility enforced in SQL
- [x] 2.3 Move `MaskKey` from `internal/routing` into `providerreg`; add store tests (PG, scoped-name conflicts, default invariants, delete constraints, cross-team invisibility, encryption round-trip) in `internal/providerreg/pgstore_test.go`

## 3. Resolver

- [x] 3.1 Add `internal/providerreg/resolver.go`: `Target` (vendor/baseURL/apiKey/model), `Resolve(userID)`, `ResolveForTeam(teamID)`, `ResolveModel(target, name)`, `VisionModel(target)`; team assignment may reference a system or team-owned provider; `ErrNoProvider` when nothing enabled
- [x] 3.2 Add resolver tests: team assignment over system provider, team assignment over team-owned provider, platform-default fallback, empty-reference fallback chain, unknown-name fail-closed, vision-model pick (system + team), no-provider error

## 4. Config + routing cleanup

- [ ] 4.1 Remove `LLM.Provider/Model/APIKey/BaseURL` and the `Vision` struct from `internal/config/config.go`; keep `LLM.ContextWindow/ThinkingBudget/RawLogDir/StreamIdleTimeout`; update config tests
- [ ] 4.2 Delete `internal/routing` (PGKeyStore team-key path, unused Router); update references (`main.go`, `internal/adminapi`)
- [ ] 4.3 Remove adminapi team-key endpoints (`listKeys`/`putKey`/`deleteKey`) and `supportedProvider`; remove their tests

## 5. Server wiring

- [ ] 5.1 In `cmd/server/main.go`: build `providerreg.Store` + `Resolver` (over pool + encryptor); construct adapters via the existing `providerSettings` factory from a `Target`
- [ ] 5.2 Replace `buildProvider`/`buildVisionProvider`/`adapterForCaller` with resolver-driven per-request resolution in `newChatLoopWithModel`; chat returns a clear error when resolution fails; the chat block no longer depends on boot-time env adapter
- [ ] 5.3 Move context-window derivation to the per-request resolved model (`LookupProfile` + model-row override + `LLM_CONTEXT_WINDOW` override)
- [ ] 5.4 Wire vision into the tool binder: resolve `VisionModel` per session and register `view_image` only when present; remove `VISION_*` usage
- [ ] 5.5 Update schedule trigger + agentdef/subagent paths to resolve model references through the resolver (fail-closed); update their tests

## 6. Admin API

- [ ] 6.1 Add `internal/adminapi/providers.go`: platform routes for system providers/models CRUD, set default provider/model, masked keys in all reads; register in `RegisterAuthed` behind `requireAdmin`
- [ ] 6.2 Add team routes behind `requireTeamRole(owner|admin)`: `GET/POST /api/teams/{id}/providers` + `PATCH/DELETE /api/teams/{id}/providers/{pid}` and their models (system providers read-only), plus `GET/PUT /api/teams/{id}/model` assignment; remove team-key routes
- [ ] 6.3 Add `internal/adminapi/providers_test.go` (platform-only guards, team-scope guards + cross-team invisibility, masked keys, default invariants 409, delete constraints, assignment constrained to enabled providers)

## 7. Frontend

- [ ] 7.1 Add `web/src/lib/providers.ts` typed client (system + team providers/models CRUD, defaults, team assignment get/put)
- [ ] 7.2 Add `web/src/components/admin/ProvidersPage.tsx` (platform: system provider list, expandable model rows, create/edit/delete, default + vision toggles) and register it in the admin nav
- [ ] 7.3 Extend `TeamDetailPage`: team provider/model management section (own providers editable, system providers read-only) + provider/model assignment picker replacing the API-key panel
- [ ] 7.4 Verify `pnpm lint` and `npx tsc --noEmit -p tsconfig.app.json`

## 8. Verification

- [ ] 8.1 `go build ./...` and `go vet ./...` clean
- [ ] 8.2 `go test ./...` green (providerreg, resolver, adminapi, chatapi, schedule, agentdef)
- [ ] 8.3 Manual smoke: create system + team providers via console → chat uses team assignment (team-owned or system); switch team model live; team key stays masked/private; `view_image` uses the assigned provider's vision model; no-provider returns clear error; `-seed-from-env` bootstraps a fresh DB
