# admin-console — tasks

## 1. Schema and configuration

- [x] 1.1 `migrations/000016_platform_role.up.sql`: add `users.platform_role TEXT NOT NULL DEFAULT 'user'` and `users.disabled_at TIMESTAMPTZ`; partial index on `platform_role = 'admin'`
- [x] 1.2 `migrations/000016_platform_role.down.sql`: drop the index and both columns
- [x] 1.3 `internal/config`: add `Identity.BootstrapAdminEmail` (`BOOTSTRAP_ADMIN_EMAIL`, default empty); document it in `.env.example`

## 2. identity: types, store, service

- [x] 2.1 `types.go`: `PlatformRole` with `PlatformRoleUser`/`PlatformRoleAdmin`; `User.PlatformRole`, `User.DisabledAt *time.Time`; `TeamMember` (user fields + role); `TeamWithRole`
- [x] 2.2 `store.go` user methods: `ListUsers(q, limit, offset) ([]User, int, error)`, `SetPlatformRole`, `SetUserDisabled`, `UpdateDisplayName`, `SetPassword`, `DeleteUser`, `CountUsers`
- [x] 2.3 `store.go` team methods: `ListTeams(q, limit, offset)`, `TeamByID`, `UpdateTeamName`, `DeleteTeam`
- [x] 2.4 `store.go` membership methods: `ListMembers`, `RoleInTeam`, `RemoveMember`, `CountOwners`, `TeamsForUser`
- [x] 2.5 `store.go` token methods: `ListTokens`, `DeleteTokenByID`, `DeleteTokensForUserExcept`
- [x] 2.6 `store.go` `CreateUser`: wrap in a transaction taking `pg_advisory_xact_lock(hashtext('nowhere.bootstrap_admin'))`; first account on an empty platform gets `admin`
- [x] 2.7 `store.go` `scanUser` and every user query: select the two new columns
- [x] 2.8 `service.go`: `Authenticate` rejects a disabled account; `Login` rejects a disabled account
- [x] 2.9 `service.go`: `PromoteByEmail(email)` — idempotent, absent email is not an error
- [x] 2.10 `service.go`: `AddMemberByEmail`, `ChangeMemberRole`, `RemoveMember` with the last-owner guard; `DisableUser` also revokes the account's tokens
- [x] 2.11 `service.go`: `ChangePassword(userID, current, next)` verifying the current password
- [x] 2.12 `http.go`: `me` DTO gains `platform_role` and `teams[]`
- [x] 2.13 Tests: first-account role, concurrent-signup single admin, `PromoteByEmail` idempotence and absent email, disabled account fails `Authenticate` and `Login`, last-owner removal and demotion refused, `ChangePassword` wrong current password refused

## 3. routing: make team keys resolvable and manageable

- [x] 3.1 `pgkeystore.go`: `Resolve` takes the provider and filters `team_api_keys.provider`; deterministic ordering retained
- [x] 3.2 `pgkeystore.go`: replace `err != sql.ErrNoRows` with `errors.Is`
- [x] 3.3 `pgkeystore.go`: `ListTeamKeys(teamID)` returning provider, masked fragment, timestamps — never the stored key
- [x] 3.4 `pgkeystore.go`: `UpsertTeamKey(teamID, provider, key)`, `DeleteTeamKey(teamID, provider)`
- [x] 3.5 Tests: provider-filtered resolve, key for another provider ignored, no team key falls back to platform key, list returns no plaintext

## 4. usage aggregation and memory scope lookup

- [x] 4.1 `internal/usage/store.go`: `PGStore` over `runs` joined to `sessions`; `Totals`, `ByUser`, `ByTeam`, `DailyForUser`, `DailyForTeam`, each taking a time range; NULL usage columns coalesce to zero
- [x] 4.2 `internal/usage`: report payload carries the team-overlap disclosure
- [x] 4.3 `internal/memory/port.go`: add `GetByID(ctx, id) (Memory, error)`
- [x] 4.4 Implement `GetByID` on `PGPort` and `MemPort`
- [x] 4.5 Tests: aggregation over runs with and without recorded usage; `GetByID` on both implementations including not-found

## 5. adminapi: the HTTP surface

- [x] 5.1 `handler.go`: `Handler` composing `identity.Service`, `routing.PGKeyStore`, `usage.PGStore`, `memory.Port`; `RegisterAuthed(mux, auth)`
- [x] 5.2 `guards.go`: `requireAdmin`; `requireTeamRole(min Role)` with platform-admin bypass; team failures return 404
- [x] 5.3 `me.go`: `GET/PATCH /api/me`, `POST /api/me/password`, `GET /api/me/usage`, `GET/DELETE /api/me/memories`, `GET/DELETE /api/me/tokens`
- [x] 5.4 `teams.go`: team CRUD, member list/add/role/remove, leave-self
- [x] 5.5 `keys.go`: `GET /api/teams/{id}/keys`, `PUT`/`DELETE` per provider
- [x] 5.6 `usage.go`: `GET /api/teams/{id}/usage`, `GET /api/admin/usage?group_by=user|team`
- [x] 5.7 `memories.go`: team-scope and platform-scope listing, deprecate, delete — each preceded by a `GetByID` scope check
- [x] 5.8 `users.go`: list/create/patch/password-reset/delete, plus the self-lockout guards
- [x] 5.9 `stats.go`: `GET /api/admin/stats` counts for the console landing view
- [x] 5.10 Tests: authorization matrix over member / team-admin / team-owner / platform-admin against every route; self-demote, self-disable, self-delete refused; cross-team memory delete refused; non-member gets 404 not 403

## 6. Wiring in cmd/server

- [x] 6.1 Register `adminapi` behind `identityHandler.RequireAuth`
- [x] 6.2 Apply `BOOTSTRAP_ADMIN_EMAIL` at startup via `PromoteByEmail`; log the outcome
- [x] 6.3 Split `buildProvider` into `buildProviderWithKey(cfg, recorder, apiKey)`
- [x] 6.4 `newChatLoop`: resolve the caller from `identity.UserFromContext(ctx)`, resolve credentials through `routing.PGKeyStore`, build the adapter for that key, fall back to the boot adapter on any error
- [x] 6.5 Replace the static `http.FileServer` with a handler that falls back to `index.html`
- [x] 6.6 Test: resolution failure falls back to the platform adapter rather than failing the request

## 7. Frontend

- [x] 7.1 `pnpm add react-router-dom`; wrap the app in `BrowserRouter` in `main.tsx`
- [x] 7.2 Extract the current chat UI into `ChatApp`; route `/` to it and `/admin/*` to the console
- [x] 7.3 `web/src/lib/me.ts`: `useMe()` fetching and caching `/api/me`
- [x] 7.4 `web/src/lib/admin.ts`: typed, token-bearing fetch wrappers for every route
- [x] 7.5 `components/admin/AdminLayout.tsx`: navigation gated on platform role and team roles
- [x] 7.6 `ProfilePage`: display name, password change, my teams, active sessions
- [x] 7.7 `TeamsPage` and `TeamDetailPage` with members / keys / usage / memories tabs
- [x] 7.8 `UsersPage`: search, paging, create, role grant, disable, password reset, delete
- [x] 7.9 `UsagePage` and `MemoriesPage` for platform scope
- [x] 7.10 Console entry point in the chat header

## 8. Verification

- [x] 8.1 `go build ./...` and `go test ./...` green
- [x] 8.2 `pnpm -C web build` (`tsc -b && vite build`) green
- [x] 8.3 `openspec validate --change admin-console`
- [x] 8.4 Manual pass against a running server: bootstrapped an administrator, created a team, set a team key and confirmed the provider rejected it (proving the key is on the request path) then confirmed fallback to the platform key on removal, read usage, exercised the authorization tiers
