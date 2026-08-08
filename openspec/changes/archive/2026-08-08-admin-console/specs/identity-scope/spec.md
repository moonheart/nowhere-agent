# identity-scope — delta for admin-console

## ADDED Requirements

### Requirement: Platform role
Every account SHALL carry a platform role of either `admin` or `user`, defaulting to `user`.
The role is a property of the account, independent of team membership, and determines whether
the account may administer the platform. The first account created on an empty platform SHALL
receive the `admin` role, so a fresh deployment always has an administrator. A deployment whose
accounts predate the role SHALL be able to designate an administrator by configuration; the
designation SHALL be idempotent and SHALL take effect without disturbing accounts it does not
name.

#### Scenario: Default role
- **WHEN** an account is created on a platform that already has accounts
- **THEN** its platform role is `user`

#### Scenario: First account bootstraps
- **WHEN** the first account on an empty platform is created
- **THEN** its platform role is `admin`

#### Scenario: Concurrent first signups produce one administrator
- **WHEN** two signups race on an empty platform
- **THEN** exactly one account receives the `admin` role

#### Scenario: Designating an administrator by configuration
- **WHEN** a deployment names an existing account's email as the bootstrap administrator
- **THEN** that account's platform role becomes `admin`, and re-applying the designation changes nothing further

#### Scenario: Designating an absent account is not an error
- **WHEN** the configured bootstrap email matches no account
- **THEN** no account is modified and startup proceeds

### Requirement: Account disablement
An account SHALL be able to be disabled without being deleted. A disabled account SHALL fail
authentication, and disabling SHALL revoke the account's outstanding tokens so that sessions
already established do not survive. Re-enabling SHALL restore the ability to authenticate with
the existing password; it SHALL NOT restore revoked tokens.

#### Scenario: Disabled account cannot authenticate
- **WHEN** a disabled account presents correct credentials
- **THEN** authentication fails

#### Scenario: Existing sessions are cut
- **WHEN** an account is disabled while holding a valid token
- **THEN** that token no longer resolves to the account

#### Scenario: Re-enabling restores login
- **WHEN** a disabled account is re-enabled
- **THEN** it authenticates with its existing password and must obtain a new token

## MODIFIED Requirements

### Requirement: Teams
The system SHALL support teams as a grouping of users for shared resources. A team SHALL have at
least one member holding the `owner` role at all times; the system SHALL reject any membership
change that would leave a team without an owner.

#### Scenario: Team membership
- **WHEN** a user is added to a team
- **THEN** they gain access to that team's shared skills and memories

#### Scenario: Team retains an owner
- **WHEN** a membership change would remove or demote a team's last owner
- **THEN** the change is rejected and the membership is left unchanged
