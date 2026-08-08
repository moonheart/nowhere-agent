# admin-console — design

## Context

Everything the console manages already exists in Postgres; almost none of it has a read or
write path. The design work here is not "what data model" — it is where the authorization
boundary lives, how the first administrator comes into being, and how the console avoids
becoming a fourth place that knows SQL.

## Decisions

### D1 — Platform role is a column on `users`, not a team-level fiction

Alternatives were an `ADMIN_EMAILS` environment allowlist and a synthetic "system team" whose
owners are administrators. The allowlist cannot be changed without a restart and gives the
console no way to grant the role; the system team conflates two orthogonal ideas (platform
authority vs. resource ownership) and would make `AccessibleScopes` lie. A column is boring,
auditable, and transferable.

`platform_role TEXT NOT NULL DEFAULT 'user'` plus `disabled_at TIMESTAMPTZ`. A partial index on
`platform_role = 'admin'` keeps "who are the admins" cheap without indexing the overwhelmingly
common `user` value.

### D2 — First-account bootstrap runs under an advisory lock

The natural implementation — `SELECT count(*) FROM users` then insert with a role — races: two
concurrent signups on an empty platform both see zero and both become administrators. There is
no row to lock, so row-level locking cannot help.

`CreateUser` therefore runs in a transaction that first takes
`pg_advisory_xact_lock(hashtext('nowhere.bootstrap_admin'))`. The lock is released at commit,
costs one round trip on every signup, and serializes only signups — acceptable for an operation
that already does a bcrypt hash.

### D3 — Existing deployments bootstrap via `BOOTSTRAP_ADMIN_EMAIL`

D2 does nothing for a database that already has accounts, which is every deployment that exists
today. The alternatives were a `cmd/admin promote` CLI and a migration that promotes the
oldest account. The CLI requires shell access to the deployment; the migration picks an account
by accident of creation order.

An environment variable, applied at startup as an idempotent `UPDATE ... WHERE email = $1`, is
recoverable (an administrator who demotes themselves can restart with the variable set) and
requires nothing beyond the config the deployment already has. An email matching no account
logs a warning and is not an error — otherwise a stale variable would prevent boot.

### D4 — Authorization is middleware in `adminapi`, composed over `identity`'s existing auth

`identity.Handler.RequireAuth` already resolves the bearer token onto the request context and
is reused verbatim. `adminapi` layers two guards over it:

- `requireAdmin` — reads the user from context, checks `PlatformRole == admin`
- `requireTeamRole(min Role)` — resolves `{id}` from the path to the caller's role in that team;
  a platform administrator short-circuits to allowed

Ordering matters: `RequireAuth` must run first, so the guards are wrapped *inside* it at
registration. Failing a team check returns 404, not 403, so a non-member cannot enumerate team
ids by probing.

The alternative — a single `requirePermission(action, resource)` policy engine — was rejected
as premature. There are two role dimensions and a dozen routes; a lookup table is more code than
the checks it replaces.

### D5 — Package layout follows the existing store-per-package convention

The repository keeps SQL next to the data it owns (`identity.Store`, `session.PGStore`,
`memory.PGPort`, `routing.PGKeyStore`) and HTTP in its own package (`chatapi`). The console
follows suit rather than becoming a monolith:

| Concern | Home | Why |
|---|---|---|
| users, teams, memberships, tokens | `internal/identity` (extend `Store`/`Service`) | it owns those tables |
| `team_api_keys` CRUD | `internal/routing` (extend `PGKeyStore`) | it already reads that table |
| `runs.usage_*` aggregation | `internal/usage` (**new**) | read-side only; does not belong to the session write path |
| memory scope checks | `internal/memory` (`Port.GetByID`) | scope lives with the memory |
| HTTP surface | `internal/adminapi` (**new**) | mirrors `chatapi`; contains no SQL |

`adminapi` depends on `identity.Service`, `routing.PGKeyStore`, `usage.PGStore`, and
`memory.Port`. It is the only new import edge.

### D6 — Team usage is the sum over members, and the report says so

`runs` records `session_id`; `sessions` records `user_id`. There is no team attribution
anywhere in the write path, so team usage can only be reconstructed as the sum over the team's
current members. Two consequences are unavoidable and are disclosed in the spec and in the
response payload rather than papered over:

- an account in several teams counts toward each, so team figures can sum above the platform total
- a member who leaves a team takes their historical usage with them

The correct fix is a `team_id` column on `runs`, written when credentials are resolved. That is
a separate change: it only becomes meaningful once D7 puts resolution on the request path, and
doing both at once would couple a reporting change to the chat hot path.

Usage is reported in tokens only. `runs` has no `model` column, so per-model breakdown and cost
estimation are impossible without another schema change; claiming a cost figure derived from a
guessed model would be worse than omitting it.

### D7 — Routing is wired by resolving from the request context, not by changing `LoopFactory`

`chatapi.LoopFactory` is `func(ctx, system) *agent.Loop`, and both call sites
(`handler.go:196`, `resume.go`) pass `r.Context()` from routes behind `RequireAuth`. The
authenticated user is therefore already reachable via `identity.UserFromContext(ctx)` inside
the factory — no signature change, no `chatapi` change at all. This was the deciding factor in
including the wiring in this change rather than deferring it: the blast radius is one closure
in `main.go`.

`buildProvider(cfg, log)` splits into `buildProviderWithKey(cfg, recorder, apiKey)`. The boot
adapter stays (it is the fallback and the dreaming worker's adapter); `newChatLoop` resolves
the caller's key and builds a per-request adapter when a team key applies. Adapters hold
`http.DefaultClient` and a handful of fields, so construction is a struct literal and
connection pooling is preserved through the shared client — no adapter cache is warranted.

Fallback is not optional: `Resolve` failing must yield the platform adapter, because the
alternative is that a Postgres hiccup takes chat down. This is covered by test.

Two defects in `PGKeyStore.Resolve` are fixed while wiring it, since wiring is what makes them
reachable: it ignores the `provider` column (returning an OpenAI key for an Anthropic call),
and it compares `err != sql.ErrNoRows` directly instead of `errors.Is`.

### D8 — Keys are write-only through the API

`GET /api/teams/{id}/keys` returns provider, a masked fragment (last four characters), and
timestamps. There is no read-back endpoint. Rotation is `PUT` with a new value. This keeps the
console from becoming a credential exfiltration path for a compromised team-admin session, and
costs nothing — nobody needs to read a key they already configured.

The stored value remains plaintext in `team_api_keys.api_key`, unchanged by this design; the
migration comment already flags encryption-at-rest as a production concern, and moving to
pgcrypto or a KMS is orthogonal to exposing management.

### D9 — Frontend gets a real router

The app is a single chat view with no router. The console has at least seven views with
nested team detail, and its URLs want to be linkable and back-button-correct.
`react-router-dom` at the top level, `/` for chat (unchanged behavior, moved into a `ChatApp`
component) and `/admin/*` for the console.

This requires SPA fallback on the Go side: `http.FileServer` 404s on `/admin/users` because no
such file exists. A small handler tries the file and falls back to `index.html`. Go 1.22+
`ServeMux` matches more specific patterns first, so `/api/...` routes are unaffected by a `GET /`
fallback.

Role-driven navigation reads `platform_role` from the extended `/api/me`. Hiding platform
sections in the client is presentation, not security — every platform route enforces
`requireAdmin` server-side regardless.

## Risks

| Risk | Mitigation |
|---|---|
| Key resolution failure breaks chat for everyone | Fallback to platform key is mandatory and tested |
| Every chat request gains a DB query | Single indexed lookup on `team_memberships`/`team_api_keys`; measured against an already-multi-query request path |
| Team usage misread as an exact partition | Stated in the spec, in `design`, and in the response payload |
| An administrator locks the platform out | Self-demote/disable/delete refused; `BOOTSTRAP_ADMIN_EMAIL` is the recovery path |
| Team-admin deletes another team's memory by id | `Port.GetByID` + scope check before every mutation |
