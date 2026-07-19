# file-tools — proposal

## Why

The agent loop already knows *how* to call tools — `Loop.toolDefs()` turns a
registry into provider tool definitions, `consume` accumulates `tool_use`
blocks, and `dispatch → Registry.CallAll` executes them and feeds results back.
But `cmd/server` builds every loop with `toolruntime.NewRegistry()` (empty) and
**no tool implementations exist**, so in practice the model has nothing to call
and the think→tool→think loop has never executed a real tool. Separately, the
sandbox layer (`internal/sandbox`) has a `Port` interface and a Docker backend,
but **nothing wires a sandbox into the server**, and there is no backend that
runs without a Docker daemon.

This change gives the agent its first real built-in tools — read/write file —
operating inside the per-session sandbox, and wires a sandbox + a per-session
tool registry into the chat path so the loop can actually use them end to end.

## What changes

- **New `LocalPort` sandbox backend** (`internal/sandbox/local.go`): a `Port`
  implementation backed by a host directory per session. Files are confined to
  the session workspace with symlink-safe path resolution. This is the default
  backend for dev/self-hosting; Docker stays available behind a config switch.
- **First built-in tools** (`internal/toolruntime/builtin`): `read_file`,
  `write_file`, `list_dir` bound to a session's `sandbox.Port` + `Handle`. All
  paths are confined to the session workspace; failures return `IsError`
  results so the model self-corrects.
- **Loop tool injection** (`agent.WithTools`): set the loop's tool registry
  after construction, once the session (and thus its sandbox handle) is known.
- **Per-session registry wiring** (`chatapi` + `cmd/server`): the `LoopFactory`
  gains the session id; the server builds a `sandbox.Manager` (local backend by
  default) and, per run, ensures the session's sandbox and registers the file
  tools bound to it.

## Capabilities touched

- `tool-runtime` — built-in tools become real and callable (spec: "Tool sources").
- `sandbox` — a local fs `Port` backend joins the Docker backend.
- `agent-loop` / `chatapi` — wiring only; no behavioural spec change (the loop
  already advertised and dispatched tools; now tools exist).

## Non-goals

- No command/shell tool (`bash`) yet — that's a separate, riskier capability.
- No MCP seam, no result-size persistence/preview, no concurrency-safety
  grading — deferred (see docs/claude-code-comparison/tool-runtime.md P1/P2).
- No egress proxy / network allowlist enforcement (sandbox 16.1) — untouched.
- No workspace solidify/materialize integration — the local backend writes the
  live session dir directly; versioned snapshots remain a workspace concern.
