# subagent — tasks

Standing constraints: write `*_test.go` for all Go code and run `go test ./...`
green before commit. No `Co-Authored-By` trailer.

## 1. Agent definitions (internal/agent/agentdef)

- [x] 1.1 `AgentDef{ Name, WhenToUse, Tools []string, DisallowedTools []string, Model string, MaxTurns int, Skills []string, System string }` and a built-in `general-purpose` (wildcard tools, empty model → inherit, generic system prompt). — `internal/agentdef/def.go`
- [x] 1.2 Markdown+frontmatter parser → `AgentDef` (body trimmed = `System`); tolerate missing optional fields; reject a doc with no `name`/`description`. — `internal/agentdef/manifest.go`
- [x] 1.3 Scoped `Store` mirroring `skill.Store`: load system/team/user scopes, merge user > team > system; `Available(scopes)` and `Resolve(name, scopes)`. — `internal/agentdef/store.go`
- [x] 1.4 Type resolution: exact match, then normalized (lower-case, strip spaces/dashes/underscores); ambiguous normalized match → error naming candidates; unknown → error listing available types.
- [x] 1.5 Tests: parse round-trip; scope override; built-in default always resolvable; normalized + ambiguous + unknown resolution paths.

## 2. Scoped registry view (internal/toolruntime)

- [x] 2.1 `Registry.Scoped(allow []string, deny []string, exclude ...string) *Registry`: allow nil/`["*"]` → all; apply deny + exclude; never mutate the receiver. Plus `Registry.Names()`. — `internal/toolruntime/scoped.go`
- [x] 2.2 Tests: allow-list, deny-list, wildcard, exclude `spawn_agent`; parent registry returns full set afterward (no mutation).

## 3. Result collapse (internal/subagent)

- [x] 3.1 `collapse(msgs []provider.Message) toolruntime.Result`: last `RoleAssistant` message's `BlockText` blocks joined; if none, walk back to the most recent assistant message with text; if none, `Content = "(subagent produced no output)"`, `IsError=false`. — `internal/subagent/collapse.go`
- [x] 3.2 Tests: final-text; tool-only-final fallback; empty → marker. (covered via spawn tests)

## 4. spawn_agent tool (internal/subagent)

- [x] 4.1 `LoopFactory func(ctx, def agentdef.AgentDef, depth int) *agent.Loop` seam (server supplies it; no import cycle).
- [x] 4.2 `SpawnTool` implementing `toolruntime.Tool`: `Name()="spawn_agent"`, schema `{ prompt (req), subagent_type (opt), description (opt) }`, `Risk()=RiskReadOnly`, sane `Timeout()`.
- [x] 4.3 `Call`: read `spawnDepth` from ctx; resolve def (default `general-purpose`); build scoped registry (allow=def.Tools + mapped skill tools, deny=def.DisallowedTools, exclude `spawn_agent` when `depth+1 >= maxDepth`); build child loop via factory with `depth+1`; seed prompt-only history; `WithTools(scoped)`; run; return `collapse(msgs)`.
- [x] 4.4 Over-cap guard: if `depth >= maxDepth`, return error result (no recursion).
- [x] 4.5 `spawnDepth` context key helpers (`withDepth`, `depthFrom`) — private. — `internal/subagent/depth.go`
- [x] 4.6 Skill mapping helper: def.Skills names → matching registered `skill_<name>_*` tool names. — `internal/subagent/skillmap.go`
- [x] 4.7 Tests (fake provider, no network): collapsed final text; unknown type; depth increments; over-cap error; allow-list scoping; nesting collapse; parent-cancel interrupt; two concurrent spawns via `CallAll`.

## 5. Wiring (cmd/server + chatapi)

- [x] 5.1 Build the `agentdef.Store` at startup.
- [x] 5.2 Provide the `SubagentFactory` closure over the provider/model/compressor so a child loop is built with `def.System`, `def.Model` (fallback parent model), `def.MaxTurns`.
- [x] 5.3 Register `spawn_agent` into each run's registry alongside the file tools (via the existing `ToolBinder` seam), passing the run's parent registry. Children share the session sandbox via the shared file-tool instances (D6).
- [x] 5.4 Config: `SUBAGENT_MAX_DEPTH` (default 3), `SUBAGENT_ENABLED` (default on).
- [x] 5.5 Chat-path end-to-end test (fake provider through the handler). — `internal/chatapi/subagent_e2e_test.go` (`TestChatSpawnsSubagent`): parent calls spawn_agent via ToolBinder, child's collapsed result lands as the parent tool_result, parent finishes with its own answer.

## 6. Activity feed (web right-panel Runs tab)

- [x] 6.1 Forward child loop events to the run panel. `agent.KindSubagent` (live-only content event); `subagent.WithSink`/`activityEmitter` forward child tool-use/done/error as `Activity{agentType,depth,phase,tool}`; run worker injects the sink → broker; attach maps it to a transient `data-subagent` frame (never a message part); web `onData` → `reportSubagentActivity` → Runs tab shows subagent runs with their tool sequence. Deterministic tests in `internal/subagent/activity_test.go`.

## 7. Validation

- [x] 7.1 `openspec validate subagent --strict` passes.
- [x] 7.2 `go test ./...` green (317 tests).
