## ADDED Requirements

### Requirement: Per-agent-type fan-out budgets
In addition to the tree-global total/concurrent budget, the runtime SHALL
support optional per-agent-type budgets: a maximum number of runs and a maximum
concurrency per agent type within one top-level run tree. A spawn exceeding its
type's total cap SHALL fail with outcome code `budget_exhausted`; a spawn
exceeding its type's concurrency cap SHALL wait for a slot, observing run
cancellation while waiting, exactly like the global semaphore. Type budgets
SHALL be configured per deployment (env-driven config) and SHALL NOT be
required: types without a configured budget are limited only by the global one.

#### Scenario: Type total cap enforced
- **WHEN** an agent type's configured run-count cap is reached within a run tree
- **THEN** the next spawn of that type fails with `budget_exhausted` naming the type and cap, while other types remain spawnable

#### Scenario: Type concurrency cap queues
- **WHEN** an agent type's configured concurrency cap is reached
- **THEN** further spawns of that type wait for a slot; a waiting spawn whose run is cancelled returns a `cancelled` error result

#### Scenario: Unconfigured type unaffected
- **WHEN** an agent type has no configured budget
- **THEN** its spawns are bounded only by the tree-global budget

### Requirement: Coalesced subagent activity streaming
Streamed child text/thinking deltas SHALL be coalesced per child run before
publication to the broker: deltas arriving within a flush window are merged
into a single activity signal, so the broker frame rate is bounded regardless
of how chatty the child is. Coalescing SHALL preserve ordering and lose no
content (merged text is the concatenation of the window's deltas); lifecycle
signals (start/tool/result/interrupted/done/error) SHALL NOT be delayed or
merged. The flush window SHALL be configurable, and a final flush SHALL fire on
the child's terminal signal.

#### Scenario: Deltas merged within a window
- **WHEN** a child emits ten text deltas inside one flush window
- **THEN** one activity signal carries their concatenation in order

#### Scenario: Lifecycle signals immediate
- **WHEN** a child starts, calls a tool, or finishes
- **THEN** those signals are published without waiting for the flush window

#### Scenario: No content loss on terminal flush
- **WHEN** a child finishes with unflushed deltas buffered
- **THEN** the terminal signal is preceded by a flush of the full buffered text
