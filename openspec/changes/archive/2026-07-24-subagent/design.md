# subagent — design

## Grounding: how Claude Code does it

Studied `E:\Git_Github\claude-code\src\tools\AgentTool`. The load-bearing ideas,
each of which maps onto machinery nowhere-agent already has:

| CC mechanism | CC source | nowhere-agent equivalent |
|---|---|---|
| `runAgent()` = an isolated `query()` (full loop, own system/tools/ctx) | `runAgent.ts:315` | `agent.Loop` built by a `LoopFactory` |
| Result collapse: last assistant **text** block → parent `tool_result` | `agentToolUtils.ts:277` (`finalizeAgentTool`) | `Loop.Run` already returns `[]provider.Message` |
| Fresh `initialMessages` (prompt only), own `readFileState` | `runAgent.ts:463` | new `history` slice per child |
| Context slimming by type (Explore/Plan drop CLAUDE.md + gitStatus) | `runAgent.ts:483,497` | child system prompt built from agent def, not copied |
| Depth guard `MAX_SUBAGENT_DEPTH = 5`, `spawnDepth` threaded | `subagentDepth.ts` | `spawnDepth` in run `context.Context` |
| Tool pool = parent pool **filtered** (allow/deny, drop spawn tool) | `agentToolUtils.ts:71,123` | scoped `toolruntime.Registry` view |
| Agent defs = scoped markdown + frontmatter, body = system prompt | `loadAgentsDir.ts:75` | mirror the skill scope loader |
| Concurrency = model emits N spawn calls in one turn | `prompt.ts:250` | `Registry.CallAll` already dispatches concurrently |

What we deliberately drop from CC (see proposal Non-goals): background agents +
cross-turn notifications, mid-run promote-to-background, worktree/remote
isolation, teammate roster/SendMessage. CC is a single-user local CLI where a
detached agent lives on an in-process task list; nowhere-agent is multi-user BS
where a run is scoped to one HTTP request. The **core value — context isolation
+ result collapse — needs none of that.**

## Key decisions

### D1: A subagent is a Tool, not a new subsystem
`spawn_agent` implements `toolruntime.Tool`. Its `Call(ctx, args)`:
1. resolves the agent definition by `subagent_type` (default `general-purpose`),
2. builds a child `agent.Loop` via an injected factory — child system prompt =
   agent def body; child registry = scoped view of the parent's pool; child
   model = agent def model (fallback parent),
3. runs `child.Run(ctx, promptHistory, sink)` to completion,
4. collapses the produced messages to the final assistant text,
5. returns it as `toolruntime.Result{Content: text}`.

The parent loop sees an ordinary tool call. `CallAll` already runs several
concurrently; `ctx` already carries cancellation + the per-tool timeout.

**Injected factory, not an import.** `spawn_agent` cannot import `chatapi`/`agent`
wiring without a cycle. It takes a `SubagentFactory func(ctx, AgentDef, depth)
*agent.Loop` supplied by the server at registration time (same shape as the
existing `LoopFactory`/`ToolBinder` seams).

### D2: Result collapse rule (mirror `finalizeAgentTool`)
Take the last `RoleAssistant` message; keep its `BlockText` blocks. If it has
none (loop ended on a `tool_use`), walk backwards to the most recent assistant
message that has text. If still nothing, return
`"(subagent produced no output)"` as a non-error result. Intermediate tool
noise never reaches the parent.

### D3: Depth guard, two locks
`spawnDepth` rides in `context.Context` (a private key). Each spawn passes
`depth+1` to the child factory. Two independent bounds, matching CC:
- **Tool-level**: at `depth >= MaxSubagentDepth` the child's scoped registry
  simply does not include `spawn_agent`. The model cannot call what it cannot
  see.
- **Call-level**: if a spawn is somehow attempted at/over the cap, `Call`
  returns an error result (self-correcting), never recurses.

Default `MaxSubagentDepth = 3` (conservative vs CC's 5 — multi-user cost
control; configurable).

### D4: Tool scoping = filtered parent pool (decision A for skills)
The child pool is the parent run's registry filtered by the agent def:
- `tools` omitted or `["*"]` → inherit the full (filtered) pool.
- `tools: [a, b]` → allow-list to those names.
- `disallowedTools` → removed after allow resolution.
- `spawn_agent` removed at max depth (D3).

**Skills need no new machinery.** A skill is already a registered `ScriptTool`
named `skill_<skill>_<script>`. An agent def's `skills: [foo]` is sugar that
adds the matching `skill_foo_*` tool names to the child's allow-list. No
preloading, no prompt expansion, no change to `skill.Engine`. This is the whole
of the skill story for subagents.

### D5: Context isolation, with type-aware slimming
Child working view = `[TextMessage(RoleUser, prompt)]` — nothing from the
parent conversation. The child's system prompt comes from the agent def body,
**not** the parent's composed system prompt, so a read-only explorer isn't
carrying the parent's memory recall / skill index. (CC's measured win: dropping
CLAUDE.md + gitStatus from Explore/Plan saved Gtok/week. Same principle: build
the child prompt for its role, don't copy the parent's.)

### D6: Sandbox sharing (v1)
The child binds the **same** `sandbox.Handle` as the parent session, so a
research subagent reads files the parent just wrote. Caveat, documented and
accepted for v1: multiple *write*-heavy subagents spawned in one turn share a
workspace and can race. Per-subagent isolation (worktree-style) is the future
mitigation; v1 targets the common read-oriented case.

### D7: Agent definitions reuse the skill scope model
Same three-scope (system/team/user) markdown loader shape as `skill.Store`,
merged user > team > system. Frontmatter → `AgentDef{ Name, WhenToUse, Tools,
DisallowedTools, Model, MaxTurns, Skills }`; body → system prompt. A single
built-in `general-purpose` def (wildcard tools, parent model) ships in code so
the capability works before any user defines an agent. Type resolution is exact
match first, then a normalized match (lower-case, strip spaces/dashes/
underscores); ambiguity errors with the candidate list.

## Data flow

```
parent Loop.Run(runCtx)                         depth = d
  └─ dispatch: Registry.CallAll(ctx, calls)     (concurrent, existing)
       └─ spawn_agent.Call(ctx, {type, prompt})
            ├─ def   = agentStore.Resolve(type | "general-purpose")
            ├─ tools = parentRegistry.Scoped(def.allow, def.deny, dropSpawn@cap)
            ├─ child = factory(ctx, def, d+1)   // system=def.body, model=def.model
            ├─ msgs  = child.Run(ctx, [user:prompt], sink)   // fresh context
            └─ return Result{ collapse(msgs) }   // last assistant text only
```

`ctx` is the parent tool-call context: cancel the parent run → child unwinds;
tool timeout bounds the child. The `sink` may forward child text/tool events to
an activity channel for the web right-panel Runs tab (optional; the durable
contract is the collapsed result).

## Testing strategy

Unit tests (`*_test.go`, `go test ./...` green before commit):
- collapse: final-text, tool-only-fallback, empty-marker.
- depth: counter increments; spawn tool absent at cap; over-cap `Call` errors.
- scoping: allow-list, deny-list, wildcard inherit, `skills:` → script tools.
- isolation: child view is prompt-only; parent registry unmutated by `Scoped`.
- type resolution: exact, normalized, unknown → error listing types.
- cancellation: parent ctx cancel interrupts a fake long child loop.
- concurrency: two spawns in one turn both return (via `CallAll`).
Agent loop drives a fake provider so no network is required (as file-tools did).
