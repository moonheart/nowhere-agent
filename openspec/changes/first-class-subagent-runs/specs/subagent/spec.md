## MODIFIED Requirements

### Requirement: Subagent spawn tool
The runtime SHALL expose a built-in tool (`spawn_agent`) that launches a child
agent to handle a delegated task autonomously. The tool SHALL accept a task
`prompt`, an optional `subagent_type`, and a short `description`. The child SHALL
run a full agent loop and return a single result to the parent. The parent loop
SHALL dispatch this tool through the same Tool interface as any other tool.
Every spawn SHALL be recorded durably as a subagent run (identity, parent run,
agent type, depth, lifecycle status, outcome code, usage, timestamps), and every
spawn result SHALL carry a structured outcome code — one of `completed`,
`depth_exceeded`, `budget_exhausted`, `timeout`, `cancelled`, `gated`,
`child_error` — so callers can distinguish retryable from terminal outcomes
without parsing prose.

#### Scenario: Spawn with an agent type
- **WHEN** the model calls `spawn_agent` with a known `subagent_type` and a prompt
- **THEN** a child loop runs with that agent definition's system prompt, scoped tools, and model, its lifecycle is recorded, and its result is returned to the parent as a tool result with outcome code `completed`

#### Scenario: Spawn without an agent type
- **WHEN** the model calls `spawn_agent` with no `subagent_type`
- **THEN** the child runs under the built-in `general-purpose` agent definition

#### Scenario: Unknown agent type
- **WHEN** the model calls `spawn_agent` with a `subagent_type` that matches no definition
- **THEN** an error result is returned listing the available agent types, and no child loop is started or recorded

#### Scenario: Depth-limited spawn reports its code
- **WHEN** a spawn is attempted at or beyond the maximum depth
- **THEN** the error result carries outcome code `depth_exceeded`

#### Scenario: Budget-limited spawn reports its code
- **WHEN** a spawn exceeds the tree-global or its agent type's budget
- **THEN** the error result carries outcome code `budget_exhausted` naming the exceeded limit

#### Scenario: Gated child reports its code
- **WHEN** a child ends awaiting client interactions it cannot receive
- **THEN** the error result carries outcome code `gated`

### Requirement: Result collapse
When a subagent finishes, only the text of its final assistant message SHALL be
returned to the parent as the tool result content. If the final assistant
message has no text (it ended on a tool call), the most recent assistant
message that has text SHALL be used. If no assistant text exists, an explicit
no-output marker SHALL be returned as a non-error result. The collapsed result
SHALL be stored on the subagent run record so a later replay can return it
without re-execution.

#### Scenario: Final assistant text returned
- **WHEN** a subagent's last assistant message contains text
- **THEN** that text is the tool result content handed to the parent

#### Scenario: Fallback when the final message is tool-only
- **WHEN** a subagent's last assistant message contains only a tool call and no text
- **THEN** the most recent assistant message with text is returned instead

#### Scenario: No output
- **WHEN** a subagent produces no assistant text at all
- **THEN** a non-error result with an explicit no-output marker is returned

#### Scenario: Collapsed result persisted
- **WHEN** a subagent run completes
- **THEN** its record holds the collapsed result content, the nested blocks, and the final usage

## ADDED Requirements

### Requirement: Idempotent spawn replay on resume
When a parent run resumes after interruption and the model re-issues a
`spawn_agent` call with the SAME tool-call id as a recorded, already-completed
subagent run in that session, the tool SHALL return the recorded outcome
(collapsed result and outcome code) WITHOUT starting a new child loop. A
recorded subagent run that ended in a non-completed outcome SHALL NOT be
replayed: its re-issue starts a fresh child under a new record. Replay SHALL
verify the prompt and agent type match the record; a mismatch SHALL start a
fresh child rather than return stale work.

#### Scenario: Completed spawn replayed after resume
- **WHEN** a resumed parent run re-issues a spawn whose tool-call id matches a completed subagent run with the same prompt and type
- **THEN** the recorded result is returned and no child loop runs

#### Scenario: Failed spawn is not replayed
- **WHEN** a resumed parent run re-issues a spawn whose tool-call id matches a `child_error` or `cancelled` record
- **THEN** a fresh child loop runs under a new subagent run record

#### Scenario: Mismatched re-issue starts fresh
- **WHEN** a re-issued spawn matches a record's tool-call id but not its prompt or agent type
- **THEN** a fresh child loop runs and the mismatch is logged
