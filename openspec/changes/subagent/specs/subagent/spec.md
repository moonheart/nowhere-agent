# subagent — spec (ADDED)

## ADDED Requirements

### Requirement: Subagent spawn tool
The runtime SHALL expose a built-in tool (`spawn_agent`) that launches a child
agent to handle a delegated task autonomously. The tool SHALL accept a task
`prompt`, an optional `subagent_type`, and a short `description`. The child SHALL
run a full agent loop and return a single result to the parent. The parent loop
SHALL dispatch this tool through the same Tool interface as any other tool.

#### Scenario: Spawn with an agent type
- **WHEN** the model calls `spawn_agent` with a known `subagent_type` and a prompt
- **THEN** a child loop runs with that agent definition's system prompt, scoped tools, and model, and its result is returned to the parent as a tool result

#### Scenario: Spawn without an agent type
- **WHEN** the model calls `spawn_agent` with no `subagent_type`
- **THEN** the child runs under the built-in `general-purpose` agent definition

#### Scenario: Unknown agent type
- **WHEN** the model calls `spawn_agent` with a `subagent_type` that matches no definition
- **THEN** an error result is returned listing the available agent types, and no child loop is started

### Requirement: Context isolation
Each subagent SHALL run over a working view seeded only with the task prompt,
independent of the parent conversation. The parent's context SHALL grow only by
the subagent's returned result, never by the subagent's intermediate transcript.
The child's system prompt SHALL come from its agent definition, not the parent's
composed system prompt.

#### Scenario: Fresh child context
- **WHEN** a subagent starts
- **THEN** its working view contains only the delegated prompt (plus any agent-definition context), with none of the parent's messages

#### Scenario: Parent context unaffected by child work
- **WHEN** a subagent makes many tool calls and produces a multi-message transcript
- **THEN** the parent loop's context receives only the collapsed result, not the child's tool calls or intermediate messages

### Requirement: Result collapse
When a subagent finishes, only the text of its final assistant message SHALL be
returned to the parent as the tool result. If the final assistant message has no
text (it ended on a tool call), the most recent assistant message that has text
SHALL be used. If no assistant text exists, an explicit no-output marker SHALL be
returned as a non-error result.

#### Scenario: Final assistant text returned
- **WHEN** a subagent's last assistant message contains text
- **THEN** that text is the tool result content handed to the parent

#### Scenario: Fallback when the final message is tool-only
- **WHEN** a subagent's last assistant message contains only a tool call and no text
- **THEN** the most recent assistant message with text is returned instead

#### Scenario: No output
- **WHEN** a subagent produces no assistant text at all
- **THEN** a non-error result with an explicit no-output marker is returned

### Requirement: Recursion depth guard
Subagent nesting SHALL be bounded by a maximum depth carried through the run
context and incremented on each spawn. At the maximum depth the child SHALL NOT
receive the `spawn_agent` tool, and any spawn attempt at or beyond the maximum
SHALL return an error result rather than recursing.

#### Scenario: Depth increments per spawn
- **WHEN** a subagent is spawned at depth `d`
- **THEN** the child runs at depth `d+1`

#### Scenario: Spawn tool withheld at maximum depth
- **WHEN** a child would run at the maximum depth
- **THEN** its scoped tool registry does not include `spawn_agent`

#### Scenario: Spawn beyond maximum depth
- **WHEN** a spawn is attempted at or beyond the maximum depth
- **THEN** an error result is returned and no further child loop is started

### Requirement: Scoped tools and model per agent definition
A subagent's available tools SHALL be the parent run's tool pool filtered by its
agent definition's allow list and deny list, with `spawn_agent` removed at the
maximum depth. An undefined or wildcard tool list SHALL inherit the full filtered
pool. The subagent SHALL use the model named by its agent definition, falling
back to the parent run's model when unset.

#### Scenario: Allow-list scoping
- **WHEN** an agent definition lists specific tools
- **THEN** the child's registry contains only those tools (that exist in the parent pool)

#### Scenario: Deny-list scoping
- **WHEN** an agent definition names disallowed tools
- **THEN** those tools are absent from the child's registry even if otherwise allowed

#### Scenario: Wildcard inherits the pool
- **WHEN** an agent definition omits a tool list or uses a wildcard
- **THEN** the child inherits the full filtered parent pool

#### Scenario: Model override and fallback
- **WHEN** an agent definition names a model
- **THEN** the child loop uses that model; otherwise it uses the parent run's model

### Requirement: Skills exposed as scoped tools
A subagent SHALL access skills only as their registered script tools within its
scoped pool. An agent definition's `skills` list SHALL add the matching skill
script tools to the child's allow list. Subagents SHALL NOT preload skill bodies
into context, and the skill engine SHALL be unchanged.

#### Scenario: Agent skills map to script tools
- **WHEN** an agent definition lists a skill by name
- **THEN** that skill's registered script tool(s) are included in the child's scoped registry

#### Scenario: No skill in scope
- **WHEN** an agent definition lists a skill that has no registered script tool
- **THEN** the child simply lacks that tool; no preloaded skill content is injected

### Requirement: Agent definitions
Agent types SHALL be defined as scoped markdown documents at system, team, and
user scopes, with frontmatter (`name`, `description`, optional `tools`,
`disallowedTools`, `model`, `maxTurns`, `skills`) and a body used as the child's
system prompt. Definitions SHALL merge across scopes with user > team > system
priority. A built-in `general-purpose` definition SHALL always be available.
Type resolution SHALL match exactly first, then by a normalized form
(case-insensitive, ignoring spaces, dashes, and underscores).

#### Scenario: Definition parsed from markdown
- **WHEN** an agent markdown document is loaded
- **THEN** its frontmatter populates the definition and its body becomes the child's system prompt

#### Scenario: Scope override
- **WHEN** an agent type is defined at multiple scopes
- **THEN** the higher-priority scope's definition is used (user overrides team overrides system)

#### Scenario: Built-in default present
- **WHEN** no user/team/system agent definitions exist
- **THEN** the `general-purpose` type is still resolvable and spawnable

#### Scenario: Normalized type match
- **WHEN** a requested type differs from a defined type only by case or separators
- **THEN** it resolves to that definition; a genuinely ambiguous request errors with the candidate list

### Requirement: Cancellation and timeout propagation
A subagent SHALL observe the parent run's cancellation and the spawn tool's
timeout. Cancelling the parent run SHALL interrupt in-flight subagents, and a
subagent exceeding the spawn tool's timeout SHALL be cancelled with an error
result.

#### Scenario: Parent cancel interrupts the child
- **WHEN** the parent run is cancelled while a subagent is running
- **THEN** the child loop observes the cancellation and stops

#### Scenario: Subagent timeout
- **WHEN** a subagent exceeds the spawn tool's timeout
- **THEN** it is cancelled and a timeout error result is returned to the parent

### Requirement: Concurrent subagents
When the model requests multiple `spawn_agent` calls in one turn, they SHALL
execute concurrently through the runtime's existing concurrent tool dispatch, and
all results SHALL be returned to the parent.

#### Scenario: Parallel spawns
- **WHEN** the model emits multiple `spawn_agent` calls in a single turn
- **THEN** the child loops run concurrently and every result is returned in call order
