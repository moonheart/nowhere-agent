# admin-console Specification

## Purpose
TBD - created by syncing change memory-consolidation. Update Purpose after archive.
## Requirements
### Requirement: Self-service consolidation
The console SHALL let an authenticated account trigger consolidation of its own
long-term memory, and SHALL report whether a pass is running and how that
account's last triggered pass went.

The route SHALL take the account from the authenticated request rather than from
a parameter, so there is no input through which one account could aim a pass at
another's sessions.

When the deployment has no consolidation worker — a deployment with no model
provider configured has none — the console SHALL degrade to hiding the control
rather than failing the view around it.

#### Scenario: Triggering from the console
- **WHEN** an authenticated account asks the console to consolidate
- **THEN** a pass over that account's own sessions begins and the request is answered without waiting for it

#### Scenario: Trigger while a pass is running
- **WHEN** an account asks to consolidate while a pass is already in flight
- **THEN** the request is refused as a conflict and no second pass starts

#### Scenario: Reporting the last pass
- **WHEN** an account's triggered pass has finished
- **THEN** the console reports what it changed — memories added, revised, retired — or the failure if it failed

#### Scenario: One account's pass is not visible to another
- **WHEN** an account has never triggered a pass
- **THEN** it is shown no pass history, including that of other accounts

#### Scenario: No worker configured
- **WHEN** the deployment has no consolidation worker
- **THEN** the consolidation control is absent and the rest of the memory view still works

### Requirement: Three-tier authorization
The management surface SHALL authorize every request at one of three tiers: **self** (any
authenticated account, acting on its own resources), **team** (authorized by the caller's
`Role` in the team named by the request path), and **platform** (authorized by
`platform_role == admin`). A platform administrator SHALL satisfy any team-tier check without
holding a membership in that team. A caller who fails a team-tier or platform-tier check SHALL
receive a rejection that does not disclose whether the named resource exists.

#### Scenario: Self tier
- **WHEN** an authenticated account requests a self-tier route
- **THEN** the request is authorized and acts only on resources owned by that account

#### Scenario: Team tier requires sufficient role
- **WHEN** a team member whose role is below the route's required role requests a team-tier route
- **THEN** the request is rejected

#### Scenario: Platform administrator bypasses team membership
- **WHEN** a platform administrator requests a team-tier route for a team they do not belong to
- **THEN** the request is authorized

#### Scenario: Non-member is not told the team exists
- **WHEN** an account that is not a member requests a team-tier route
- **THEN** the response does not distinguish "team does not exist" from "you are not a member"

#### Scenario: Ordinary account cannot reach platform routes
- **WHEN** an account whose `platform_role` is `user` requests a platform-tier route
- **THEN** the request is rejected

### Requirement: User lifecycle administration
A platform administrator SHALL be able to list accounts (filtered by a search term and paged),
create an account with an initial password, change an account's display name, grant or revoke
the platform-administrator role, disable or re-enable an account, reset an account's password,
and delete an account.

#### Scenario: Listing accounts
- **WHEN** a platform administrator lists accounts with a search term
- **THEN** accounts matching the term by email or display name are returned with their platform role and disabled state, together with the total count for paging

#### Scenario: Creating an account
- **WHEN** a platform administrator creates an account with an email and an initial password
- **THEN** the account exists and can authenticate with that password

#### Scenario: Granting the administrator role
- **WHEN** a platform administrator grants the platform-administrator role to another account
- **THEN** that account passes platform-tier authorization on its next request

#### Scenario: Resetting a password
- **WHEN** a platform administrator resets an account's password
- **THEN** the old password no longer authenticates and the new one does

### Requirement: Administrators cannot lock themselves out
A platform administrator SHALL NOT be able to revoke their own administrator role, disable
their own account, or delete their own account. Such a request SHALL be rejected with an
explanatory error and SHALL leave the account unchanged.

#### Scenario: Self-demotion refused
- **WHEN** a platform administrator revokes their own administrator role
- **THEN** the request is rejected and the account retains the role

#### Scenario: Self-disable refused
- **WHEN** a platform administrator disables their own account
- **THEN** the request is rejected and the account remains enabled

#### Scenario: Self-deletion refused
- **WHEN** a platform administrator deletes their own account
- **THEN** the request is rejected and the account still exists

### Requirement: Team and membership administration
Any authenticated account SHALL be able to create a team, becoming its owner, and to list the
teams it belongs to together with its role in each. A team owner or team administrator SHALL be
able to rename the team, add an existing account as a member by email at a chosen role, and
remove a member. Changing a member's role SHALL require the owner role. Deleting a team SHALL
require the owner role. Any member SHALL be able to remove themselves from a team, which
constitutes leaving it.

#### Scenario: Creating a team
- **WHEN** an authenticated account creates a team
- **THEN** the team exists and the creator is its owner

#### Scenario: Listing my teams
- **WHEN** an account lists its teams
- **THEN** each team is returned with the caller's role in it

#### Scenario: Adding a member by email
- **WHEN** a team owner or administrator adds an existing account by email at a role
- **THEN** that account becomes a member of the team at that role

#### Scenario: Adding an unknown email
- **WHEN** a team owner adds an email that matches no account
- **THEN** the request is rejected and no membership is created

#### Scenario: Role change requires ownership
- **WHEN** a team administrator who is not an owner changes another member's role
- **THEN** the request is rejected

#### Scenario: Leaving a team
- **WHEN** a member removes themselves from a team
- **THEN** their membership is removed

### Requirement: A team always retains an owner
The system SHALL reject any operation that would leave a team with no owner — removing the last
owner, or demoting the last owner to a lesser role. The membership SHALL be left unchanged.

#### Scenario: Removing the last owner refused
- **WHEN** an operation would remove a team's only owner
- **THEN** the request is rejected and the membership is unchanged

#### Scenario: Demoting the last owner refused
- **WHEN** an operation would demote a team's only owner to administrator or member
- **THEN** the request is rejected and the role is unchanged

#### Scenario: Removing an owner when others remain
- **WHEN** a team has more than one owner and one is removed
- **THEN** the removal succeeds

### Requirement: Team provider credential management
> **SUPERSEDED (deprecated):** this requirement describes the deleted
> `team_api_keys` mechanism (migration 000028 drops the table). Team
> credentials are now team-scoped providers in the provider registry (change
> provider-registry): a team owner/administrator manages the team's own
> providers and models; provider keys are encrypted at rest, masked in
> listings, and never returned in plaintext. Keep the archived description
> below for historical context only.

A team owner or team administrator SHALL be able to list the providers for which the team has
configured a key, set or rotate the key for a provider, and delete it. Listing SHALL NOT return
a stored key in plaintext; it SHALL return only the provider, a masked fragment sufficient to
distinguish one key from another, and the times the record was created and last updated.

#### Scenario: Key is never returned in plaintext
- **WHEN** a team administrator lists the team's provider keys
- **THEN** each entry carries the provider and a masked fragment, and no response field contains the stored key

#### Scenario: Rotating a key
- **WHEN** a team administrator sets a key for a provider that already has one
- **THEN** the stored key is replaced and the record's updated time advances

#### Scenario: Members cannot read keys
- **WHEN** a member whose role is neither owner nor administrator lists the team's keys
- **THEN** the request is rejected

### Requirement: Usage reporting
The system SHALL report token usage — input, output, cache-read, and cache-write — aggregated
from the persisted per-run counts. An account SHALL be able to read its own usage; a team owner
or administrator SHALL be able to read their team's; a platform administrator SHALL be able to
read platform-wide usage grouped by account or by team. Every report SHALL accept a time range.

A team's usage SHALL be computed as the sum over the usage of its members, because a run
records the account that owns its session and carries no team attribution. An account belonging
to several teams therefore counts toward each of them, and the sum of all team figures MAY
exceed the platform total. Reports SHALL disclose this so the figures are not read as an exact
partition.

#### Scenario: Own usage
- **WHEN** an account requests its usage for a time range
- **THEN** the response totals the input, output, cache-read, and cache-write tokens of runs owned by that account within the range

#### Scenario: Team usage sums its members
- **WHEN** a team administrator requests the team's usage
- **THEN** the response totals the usage of every current member of the team

#### Scenario: Overlap is disclosed
- **WHEN** a usage report is grouped by team
- **THEN** the response states that an account in several teams is counted toward each

#### Scenario: Runs with no recorded usage
- **WHEN** a run has no recorded token counts
- **THEN** it contributes zero rather than causing the report to fail

### Requirement: Scoped memory administration
A memory SHALL be readable and removable only by a caller entitled to its scope: an account for
its own user-scoped memories, a team owner or administrator for their team's, and a platform
administrator for any scope including system. Before deprecating or deleting a memory the
system SHALL resolve the memory and verify its scope against the caller's entitlement, rather
than acting on an identifier alone.

Memory listings SHALL hide superseded memories by default and make them available on request,
reporting how many are hidden. A superseded memory is excluded from recall and is therefore not
part of what the agent knows; presenting it beside live memories misstates the store, and
consolidation retires enough memories that the superseded ones can outnumber the rest.

#### Scenario: Listing by scope
- **WHEN** a caller lists memories for a scope they are entitled to
- **THEN** the memories in that scope are returned

#### Scenario: Superseded memories are hidden by default
- **WHEN** a scope contains both live and superseded memories
- **THEN** only the live ones are listed, and the number of superseded ones is reported

#### Scenario: Superseded memories remain reachable
- **WHEN** a caller asks to see superseded memories
- **THEN** they are listed alongside the live ones and marked as superseded

#### Scenario: Every memory in a scope is superseded
- **WHEN** a scope's memories have all been superseded
- **THEN** the view says so rather than reporting that nothing was ever remembered

#### Scenario: Cross-team deletion refused
- **WHEN** a team administrator deletes a memory identified by a valid id that belongs to a different team
- **THEN** the scope check fails and the memory is not deleted

#### Scenario: Deprecating retains the record
- **WHEN** a memory is deprecated
- **THEN** it is excluded from recall but the record remains

#### Scenario: System-scope memories are platform-only
- **WHEN** a caller who is not a platform administrator lists or deletes system-scope memories
- **THEN** the request is rejected

### Requirement: Self-service account settings
An authenticated account SHALL be able to read its own profile including its platform role and
its team memberships, change its display name, change its password by supplying the current
one, list its active authentication tokens, revoke an individual token, and revoke every token
other than the one authenticating the request.

#### Scenario: Profile carries role and teams
- **WHEN** an account reads its profile
- **THEN** the response includes its platform role and every team it belongs to with its role in each

#### Scenario: Password change requires the current password
- **WHEN** an account changes its password supplying an incorrect current password
- **THEN** the request is rejected and the password is unchanged

#### Scenario: Revoking other sessions
- **WHEN** an account revokes every token other than the current one
- **THEN** those tokens no longer authenticate and the current request's token still does

### Requirement: Console routing and deep links
The management console SHALL be reachable at its own client-side route, separate from the chat
view, and the server SHALL serve the application shell for any console path so that a deep link
loads directly. API routes SHALL take precedence over this fallback.

#### Scenario: Deep link loads
- **WHEN** a browser requests a console path directly
- **THEN** the application shell is served and the console renders that path

#### Scenario: API routes unaffected
- **WHEN** a request targets an API route
- **THEN** it is handled by that route and never by the application-shell fallback

#### Scenario: Console entries reflect role
- **WHEN** the console renders for an account that is not a platform administrator
- **THEN** platform-tier sections are not offered
