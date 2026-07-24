# tool-runtime — delta for subagent

## ADDED Requirements

### Requirement: Scoped tool registry view
The registry SHALL be able to produce a filtered view of its tools for a child
run, selecting by an allow list and/or removing a deny list and/or excluding
named tools, without mutating the parent registry. The parent registry's tool
set SHALL be unchanged by producing a scoped view.

#### Scenario: Allow-list view
- **WHEN** a scoped view is requested with an allow list
- **THEN** the view contains only the named tools that exist in the parent registry

#### Scenario: Deny-list view
- **WHEN** a scoped view is requested with a deny list
- **THEN** the named tools are absent from the view even if otherwise allowed

#### Scenario: Wildcard view
- **WHEN** a scoped view is requested with no allow list (or a wildcard)
- **THEN** the view contains all parent tools minus any denied or excluded ones

#### Scenario: Parent registry unaffected
- **WHEN** a scoped view is produced
- **THEN** the parent registry still returns its full, unfiltered tool set
