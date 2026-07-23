# subagent — proposal

## Why

The agent loop can already call tools, read/write files in its sandbox, and load
skills — but everything runs in **one** conversation with **one** growing
context. Two needs push past that:

- **Context isolation.** Broad research ("map how sessions are persisted") or a
  self-contained sub-task ("review this migration") pours tool noise — dozens of
  file reads, greps, intermediate results — into the main context. That noise
  crowds out the actual work and drives compression early.
- **Delegation.** Some work wants a *different* posture than the main agent: a
  read-only explorer, a stricter reviewer, a cheaper model for a mechanical
  pass. Today the loop has one system prompt and one tool pool for the whole
  run.

The fix is the pattern Claude Code uses for its `Task`/`Agent` tool: a subagent
is **not a new engine** — it is one more `toolruntime.Tool` whose `Call` runs a
*child* `agent.Loop` with a fresh context, its own system prompt, and a scoped
tool pool, then collapses the child's entire transcript down to a single result
handed back to the parent. The parent's context grows by one summary, not by the
child's whole run.

This is the same origin-agnostic seam that already carries built-in tools and
skill scripts (`tool-runtime`): the loop never learns that a tool call spun up
another loop.

## What changes

- **New capability `subagent`**: a built-in `spawn_agent` tool that launches a
  child `agent.Loop`, runs it to completion over a prompt-only working view, and
  returns the child's final assistant text as the tool result.
- **Agent definitions** (`internal/agent/agentdef` or reuse of the skill scope
  loader): scoped markdown (system/team/user) with frontmatter — `name`,
  `description`, `tools`, `disallowedTools`, `model`, `maxTurns`, `skills` — and
  a body used as the child's system prompt. Merged user > team > system, exactly
  like skills. A built-in `general-purpose` definition is the default.
- **Recursion depth guard**: a spawn-depth counter carried through the run
  context; a maximum depth beyond which the child does not receive the
  `spawn_agent` tool and a spawn attempt errors.
- **Scoped tool registry view** (`tool-runtime`): the registry can produce a
  filtered view (allow/deny + spawn-tool exclusion) for a child run without
  mutating the parent registry.
- **Result collapse**: the child's final assistant text becomes the parent's
  tool result; a tool-only final message falls back to the last assistant text;
  empty output returns an explicit marker.

## Capabilities touched

- `subagent` — new capability (spawn tool, isolation, result collapse, depth
  guard, agent definitions, tool/model scoping).
- `tool-runtime` — additive: a scoped registry view for child runs. No change to
  existing tool-source, schema, execution, or confinement requirements.

## Non-goals (deferred)

- **Background / async subagents.** v1 is foreground-synchronous only: the
  spawn tool blocks until the child finishes. Detached sub-runs, cross-turn
  completion notifications, and "promote to background mid-run" are future work
  and would need to integrate with `session.Runtime` (multi-user, not CLI).
- **Isolated sandboxes / worktrees per subagent.** v1 children share the parent
  session's sandbox handle (a read-oriented explorer sees the parent's files).
  Per-subagent isolation is deferred.
- **Teammate collaboration** (named roster, inter-agent messaging) — future.
- **Prompt-form skills.** Skills stay script-form (`ScriptTool`); a subagent
  "uses a skill" by having that skill's script tool in its scoped pool. No skill
  preloading / prompt expansion, and no change to the skill engine.
- **Permission wiring for spawn.** The spawn tool declares a `Risk`, but
  `permission.Checker` enforcement remains unwired (tracked separately); child
  tools carry their own existing risk classification.
