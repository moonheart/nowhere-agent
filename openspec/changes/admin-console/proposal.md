# admin-console — proposal

## Why

The platform has users, teams, memberships, team-scoped provider keys, and per-run token
accounting — all of it in Postgres, none of it reachable. `internal/identity` exposes exactly
four endpoints (`signup`/`login`/`logout`/`me`); every team operation that `identity.Store`
implements has no HTTP surface at all, so a team can only be created by hand-writing SQL. There
is no notion of a **platform administrator**: `users` has no role column, so no account can
administer another. And `team_api_keys` is a dead table — `internal/routing` is never imported
by `cmd/server`, which builds one global adapter from `LLM_API_KEY`, so a team's configured key
has zero effect on its model calls.

The result is a multi-tenant product where none of the three roles it was designed around —
**system administrator, team administrator, team member** — can do anything. This change gives
each of them a console.

## What Changes

- **Platform role.** `users` gains `platform_role` (`user` | `admin`) and `disabled_at`. The
  first account to sign up becomes `admin`; existing deployments bootstrap one via
  `BOOTSTRAP_ADMIN_EMAIL`, applied idempotently at startup. Disabling an account revokes its
  tokens and makes authentication fail immediately.
- **New `admin-console` capability.** An authenticated HTTP surface split three ways:
  - *self-service* (`/api/me/**`) — profile, password, my usage, my memories, my login sessions
  - *team* (`/api/teams/**`) — team CRUD, member add/role/remove, provider keys, team usage,
    team memories; authorized by the caller's role **in that team**
  - *platform* (`/api/admin/**`) — user lifecycle, role grants, all teams, platform-wide usage,
    any-scope memories; authorized by `platform_role == admin`
- **Lock-out guards** as spec'd behavior, not incidental checks: an administrator cannot
  demote, disable, or delete themselves, and a team's last owner cannot be removed or demoted.
- **Team provider keys become real.** `cmd/server` wires `routing.PGKeyStore` into the chat
  request path, so a team key resolved for the calling user is the key its model calls use.
  Credential resolution is additionally filtered by provider (today it returns any team key
  regardless of which provider it belongs to) and falls back to the platform key on any
  resolution failure.
- **Usage reporting.** A read-side aggregation over the `runs.usage_*` columns, grouped by user,
  by team, and by day. Team figures are the sum over the team's members — `runs` carries no
  `team_id`, so a user in several teams counts toward each. This approximation is stated in the
  spec rather than hidden.
- **Memory administration.** `memory.Port` gains `GetByID` so a delete or deprecate can verify
  the memory's scope belongs to the caller before acting, instead of trusting a UUID.
- **Frontend.** `react-router-dom` is introduced; the app splits into `/` (chat, unchanged) and
  `/admin/*` (console). The Go static handler gains SPA fallback so deep links resolve.

## Capabilities

### New Capabilities
- `admin-console`: the three-tier management surface — authorization matrix (self / team role /
  platform role), user lifecycle, team and membership administration, team credential
  management, usage reporting, and scoped memory administration.

### Modified Capabilities
- `identity-scope`: adds the platform-role concept (administrator vs ordinary account), account
  disablement with token revocation, and the membership-integrity constraint that a team always
  retains at least one owner.
- `model-routing`: the existing "team key override" requirement is currently unmet in the
  running system; this change puts credential resolution on the request path and scopes it to
  the provider actually being called.

## Impact

- **Schema**: `migrations/000016_platform_role.{up,down}.sql` — two additive columns on `users`
  plus a partial index. No data rewrite; safely reversible.
- **Go packages**: `internal/identity` (types, store, service, `me` DTO), `internal/routing`
  (provider-filtered resolve + key CRUD), `internal/memory` (`Port.GetByID`, two
  implementations), `internal/usage` (**new**), `internal/adminapi` (**new**, HTTP only),
  `internal/config` (`BOOTSTRAP_ADMIN_EMAIL`), `cmd/server/main.go` (route registration,
  routing wiring, SPA fallback, bootstrap promotion).
- **Chat request path**: each chat request now performs one `team_api_keys` lookup to resolve
  credentials. A failed lookup must fall back to the platform key — a regression here breaks
  chat outright, so it is covered by test.
- **Frontend**: new dependency `react-router-dom`; `App.tsx` splits into a chat route and the
  console; new `web/src/components/admin/**` and `web/src/lib/{me,admin}.ts`.
- **Out of scope**: email invitations, SSO, audit logging, cost estimation (no `runs.model`
  column exists, so usage is reported in tokens only), global session/run monitoring, skill
  administration (the skill store is still process-memory seeded from disk), and the routing
  package's failover and quota features.
