# dreaming — delta for memory-consolidation

## MODIFIED Requirements

### Requirement: Reorganization
The worker SHALL consolidate a session batch against the scope's complete live memory set in a
single operation. The consolidation SHALL be given the new material and every live memory in the
scope, and SHALL be able to revise an existing memory in place, add a new one, or retire one that
has been merged or superseded. Retiring a memory SHALL deprecate it rather than erase it.

Consolidation SHALL address existing memories by a stable identifier supplied with them, not by
matching their text. An identifier the system does not recognize SHALL be ignored and recorded;
it SHALL NOT be resolved to a memory by approximate or substring matching.

Consolidation SHALL be a single operation per batch, independent of how many facts the batch
yielded, so its cost does not scale with the number of extracted facts.

#### Scenario: Consolidation sees the whole live set
- **WHEN** the worker consolidates a batch for a scope
- **THEN** every live memory in that scope is available to the operation

#### Scenario: An existing memory is revised in place
- **WHEN** consolidation determines an existing memory should now read differently
- **THEN** that memory's content is updated and it keeps its identity, rather than being duplicated by a new memory

#### Scenario: Two memories are merged
- **WHEN** consolidation merges one memory into another
- **THEN** the surviving memory carries the merged content and the absorbed memory is deprecated

#### Scenario: Unknown identifier is ignored
- **WHEN** consolidation references an identifier that matches no memory in the scope
- **THEN** no memory is modified by that reference and the occurrence is recorded

#### Scenario: Cost does not scale with fact count
- **WHEN** a batch yields many facts
- **THEN** consolidation is still performed as one operation over the live set

#### Scenario: Retirement is reversible until purged
- **WHEN** consolidation retires a memory
- **THEN** the memory is deprecated and excluded from recall, and its record remains until the retention window closes

### Requirement: Reflection
The worker SHALL derive higher-level cross-session patterns about the user. An insight SHALL
describe the user — who they are, what they prefer, own, or are working on. An insight SHALL NOT
describe the assistant's behaviour, the conversation as an artifact, or the memory system itself.

Insights SHALL be subject to the live-memory cap for their kind, so the number of live insights is
bounded regardless of how many sessions occur.

#### Scenario: Cross-session insight
- **WHEN** multiple episodes reveal a recurring pattern about the user
- **THEN** the worker stores an insight capturing that pattern

#### Scenario: Self-referential insight is not stored
- **WHEN** a candidate insight describes the memory system, the consolidation process, or the assistant rather than the user
- **THEN** it is not stored as a memory

#### Scenario: Insight count stays bounded
- **WHEN** consolidation runs repeatedly over many sessions
- **THEN** the number of live insights in a scope never exceeds the configured cap

### Requirement: Budget control
Dreaming's LLM usage SHALL be bounded by a configurable budget. Each stage that spends tokens
SHALL verify the remaining allowance before spending it, so the budget bounds work within a
session batch and not only between batches.

When a batch's consolidation is skipped because the allowance is exhausted, the batch's progress
marker SHALL NOT advance, so the episode is consolidated by a later pass rather than being
consumed without being learned from.

#### Scenario: Budget cap
- **WHEN** a run would exceed the configured LLM budget
- **THEN** the worker defers or truncates work to stay within budget

#### Scenario: A single batch cannot overrun the budget
- **WHEN** one session batch's work would exceed the remaining allowance
- **THEN** the stage that would exceed it is not performed, rather than being discovered after the fact

#### Scenario: A skipped batch is retried, not lost
- **WHEN** consolidation is skipped for a batch because the allowance is exhausted
- **THEN** that batch's progress marker is unchanged and a later pass consolidates the same episodes

## ADDED Requirements

### Requirement: Bounded memory store
Each scope SHALL have a configurable maximum number of live memories per kind. The bound SHALL
count live memories only; deprecated memories SHALL NOT count toward it.

Enforcement SHALL be two-stage. Consolidation SHALL be told each cap and the current count and be
given the opportunity to merge or retire memories so the result fits. After consolidation's
operations are applied, any kind still exceeding its cap SHALL be brought under it by deprecating
its oldest live memories. The bound SHALL hold regardless of what consolidation returns.

Caps SHALL be per kind rather than a single total, so a kind that generates freely cannot consume
the allowance of a kind that generates rarely.

#### Scenario: Cap is enforced when consolidation complies
- **WHEN** consolidation merges memories so a kind fits its cap
- **THEN** the merged result is stored and no eviction is needed

#### Scenario: Cap is enforced when consolidation does not comply
- **WHEN** consolidation returns operations that leave a kind above its cap
- **THEN** the oldest live memories of that kind are deprecated until the cap holds

#### Scenario: Deprecated memories do not consume the cap
- **WHEN** a scope holds many deprecated memories of a kind
- **THEN** they do not count toward that kind's cap and do not cause eviction of live memories

#### Scenario: One kind cannot crowd out another
- **WHEN** one kind is at its cap
- **THEN** other kinds retain their full allowance

### Requirement: Purge of retired memories
Deprecated memories SHALL be permanently deleted after a configurable retention period, so the
store does not grow without bound in memories that can never be recalled.

#### Scenario: Retired memory is purged after the window
- **WHEN** a memory has been deprecated for longer than the retention period
- **THEN** it is permanently deleted

#### Scenario: Recently retired memory is kept
- **WHEN** a memory was deprecated within the retention period
- **THEN** it is retained and remains excluded from recall

### Requirement: Consolidation fidelity
Consolidation SHALL only reorganize what it is given. A memory it writes SHALL be
supported by the memories it is rewriting or by the new material from the
episode; it SHALL NOT introduce a name, quantity, date, place, or event that
appears in neither.

In particular, consolidation SHALL NOT invent a change of state to reconcile
sources that disagree. Two memories in conflict are two memories in conflict —
not evidence that the user corrected themselves. When nothing resolves a
conflict, the more recent wording SHALL be kept and the other retired, rather
than a third version being synthesized.

#### Scenario: Merging duplicates preserves their content
- **WHEN** several memories state the same fact in different wordings or languages
- **THEN** the merged memory states that same fact, and introduces no detail absent from all of them

#### Scenario: No invented reconciliation
- **WHEN** two memories disagree and nothing in the new material resolves it
- **THEN** consolidation keeps one and retires the other, without writing that the user changed or corrected anything

### Requirement: Consolidation without new episodes
Consolidation SHALL be able to run over an existing store with no new episode
material, reviewing the store on its own terms: merging duplicates, retiring what
time has made stale, and holding the caps.

A request to consolidate SHALL NOT be a no-op merely because there are no
unconsolidated sessions. A store can need consolidating without a conversation
having produced it.

#### Scenario: Consolidating with no unconsolidated sessions
- **WHEN** consolidation is requested for an account whose sessions are all already consolidated
- **THEN** the existing store is reviewed and duplicates within it are merged

#### Scenario: Nothing to review
- **WHEN** consolidation is requested for an account with no memories
- **THEN** no model call is made

#### Scenario: Reported distinctly
- **WHEN** a pass reviewed the existing store rather than learning from new episodes
- **THEN** it is reported as such, not as having found nothing to do

### Requirement: On-demand consolidation
An account SHALL be able to trigger consolidation of its own sessions on demand,
without waiting for the schedule. The trigger SHALL be scoped to the requesting
account: it SHALL NOT read, consolidate, or spend tokens on any other account's
sessions.

Consolidation SHALL remain available when the scheduled worker is disabled.
Disabling the schedule SHALL govern *when* consolidation happens, not *whether*
the system can consolidate at all.

A triggered pass SHALL run independently of the request that started it, so the
caller is not held for its duration.

At most one consolidation pass SHALL run at a time across the system, whether
scheduled or triggered. A caller who triggers while a pass is in flight SHALL be
told it is already running rather than starting a second one. A scheduled tick
that arrives while a pass is in flight SHALL be skipped rather than treated as a
failure.

The system SHALL report, per account, whether a pass is in flight and the
outcome of that account's most recent triggered pass — including its failure,
when it failed.

#### Scenario: Triggering consolidates only the requester's sessions
- **WHEN** an account triggers consolidation
- **THEN** only that account's sessions are read and consolidated

#### Scenario: Available with the schedule disabled
- **WHEN** the scheduled worker is disabled
- **THEN** an account can still trigger consolidation and it runs normally

#### Scenario: The caller is not held for the pass
- **WHEN** an account triggers consolidation
- **THEN** the request is answered immediately and the pass continues afterwards

#### Scenario: Concurrent trigger is refused
- **WHEN** a consolidation pass is in flight and another trigger arrives
- **THEN** the second trigger does not start a pass and the caller is told one is already running

#### Scenario: Scheduled tick during a manual pass is skipped
- **WHEN** the schedule fires while a triggered pass is in flight
- **THEN** the tick is skipped without being reported as a failure

#### Scenario: A failed pass does not block later ones
- **WHEN** a pass fails
- **THEN** the failure is reported to the account that triggered it and a subsequent trigger is accepted

#### Scenario: One account's pass is not visible to another
- **WHEN** an account has never triggered a pass
- **THEN** it is shown no pass history, including that of other accounts
