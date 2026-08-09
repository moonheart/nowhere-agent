## ADDED Requirements

### Requirement: Agent definitions management page
The console SHALL provide an agent-definitions page where an authenticated
account manages definitions at the tiers it is authorized for: its own
definitions, team definitions for teams where its role suffices, and system
definitions when it is a platform administrator. The page SHALL list
definitions with name, scope, when-to-use summary, and model/tool overrides;
support creating and editing definitions as markdown documents with
frontmatter assistance; and support deletion with confirmation. Tiers the
caller cannot authorize SHALL be hidden rather than shown disabled.

#### Scenario: Self definitions manageable
- **WHEN** an authenticated account opens the agent-definitions page
- **THEN** it sees and can create, edit, and delete its own definitions

#### Scenario: Team section gated by role
- **WHEN** an account with a sufficient team role opens the page
- **THEN** that team's definitions section is present and editable; for teams where the role is insufficient the section is absent

#### Scenario: System section admin-only
- **WHEN** a platform administrator opens the page
- **THEN** the system-scope definitions section is present; a non-admin never sees it

#### Scenario: Write validation surfaced
- **WHEN** an edit fails server-side validation (bad frontmatter, empty name/body)
- **THEN** the page shows the validation error and keeps the editor's content for correction
