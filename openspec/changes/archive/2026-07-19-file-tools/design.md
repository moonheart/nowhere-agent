# file-tools — design

## Context

The loop's tool plumbing is complete and tested (`internal/agent/loop.go`:
`toolDefs`, `consume` accumulating `tool_use`, `dispatch → CallAll`, results
re-fed as `tool_result`). What's missing is everything downstream of the empty
registry the server passes in. This design fills that gap with the smallest set
of pieces that make a real think→tool→think round-trip work against real
storage, while keeping every seam (`sandbox.Port`, `toolruntime.Tool`,
`agent.Loop`) unchanged in shape.

## Decisions

### D1 — Tools are bound to a session's sandbox, not constructed globally

A file tool needs two things that only exist per-session: the `sandbox.Handle`
and a confined workspace root. So tools are **built per run**, once the session
id is known, and registered into a fresh per-run `toolruntime.Registry`. They
are not process-global singletons. This keeps multi-tenant isolation structural:
one session's tools physically cannot address another session's files because
each tool closes over its own `Handle`/root.

`builtin.FileTools(sb sandbox.Port, h sandbox.Handle) []toolruntime.Tool` is the
single construction point. The loop stays origin-agnostic (spec: unified
interface).

### D2 — Loop tool injection via `WithTools`, mirroring `WithImages`

The handler already resolves the session id *after* building the loop
(`newLoop(ctx, system)`). The existing pattern for session-bound config is
`Loop.WithImages(resolver)`, called post-construction in `serveChat`. We add the
symmetric `Loop.WithTools(reg *toolruntime.Registry)`. Default remains an empty
registry, so tests and the no-sandbox path keep working unchanged.

Rationale for a setter over changing `agent.New`: `agent.New` is called in many
tests with a registry already; threading "registry now, real one later" through
the constructor would complicate the common case. A setter matches the codebase
idiom (`WithImages`, `WithRuntime`, `WithMessageStore`).

### D3 — Path confinement lives in the sandbox backend, tools stay thin

File tools accept a model-supplied `path`. The danger is `../../etc/passwd` or a
symlink pointing outside the workspace. Confinement must be **structural, not a
string check**, so it lives in `LocalPort.resolve(path)`:

1. Clean the path; reject absolute paths and any `..` that escapes the root.
2. Join onto the session root.
3. `filepath.EvalSymlinks` the result AND the root, then require the resolved
   file to be `filepath.Rel`-contained under the resolved root. This defeats a
   symlink inside the workspace that points outside it (the trick a plain
   `strings.HasPrefix` check misses).

Tools just hand their `path` to `Port.ReadFile/WriteFile/ListDir`; the port is
the security boundary (matches the package doc: "the interface hides
local-vs-remote"). The Docker backend confines by container construction
(bind-mount), the local backend confines by `resolve`.

### D4 — LocalPort: host-dir backend, `Manager` unchanged

`LocalPort{root string}` implements `Port`. `Create(sessionID, opts)` makes
`<root>/<sessionID>/` (or uses `opts.WorkspaceDir` when set), returning a
`Handle{ID: "local-<sessionID>"}`. Read/Write/List operate on `resolve(path)`.
`Exec` runs via `os/exec` with `cmd.Dir` set to the workspace (unused by the
file tools but completes the interface; the network policy is a documented
no-op for the local backend). `Destroy` removes the workspace dir.

The existing `sandbox.Manager` (Ensure/deferred-stop/Sweep) works over any
`Port`, so it is reused as-is — per-session sandbox lifecycle comes free.

### D5 — `Manager.Ensure` per run; registry built on the handle

In `serveChat`, after `resolveSession`, the handler (via the loop factory)
calls `mgr.Ensure(ctx, sessID, opts)` to get a live `Handle`, builds the file
tools bound to it, registers them, and `loop.WithTools(reg)`. Because `Ensure`
is idempotent for a running session, repeated runs on one session share one
sandbox/workspace — files written in run N are readable in run N+1 (the whole
point of a persistent workspace).

### D6 — Tool wiring via a `ToolBinder` functional option

Rather than change `LoopFactory`'s signature (which would churn every test call
site), the handler gains a `ToolBinder func(ctx, loop, sessionID)` option set
via `WithToolBinder`. After `resolveSession` yields the session id, `serveChat`
invokes the binder alongside the existing `WithImages` call. The server's
binder ensures the session's sandbox and registers its file tools into the
loop's registry via `WithTools`. When no binder is configured (default), the
loop runs tool-free exactly as before.

### D7 — Config: `SANDBOX_BACKEND` + `SANDBOX_WORKSPACE_DIR`

`SANDBOX_BACKEND` = `off` (default) | `local` | `docker`. `off` preserves today's
behaviour exactly (empty registry, no sandbox) so the change is opt-in and the
test suite is undisturbed. `local` uses `SANDBOX_WORKSPACE_DIR` (falling back to
`WORKSPACE_DIR`) as the session root. `docker` keeps the existing `DockerPort`.

### D8 — Errors are tool-results, not fatal

Read of a missing file, a write to a read-only path, a path-escape attempt: all
become `Result{IsError: true, Content: "<reason>"}`. The loop appends them as
`tool_result` blocks and the model self-corrects (spec: error feedback for
self-correction). A path-escape is reported as an error result, never silently
remapped.

## Data flow (one run with tools)

```
serveChat
  └─ resolveSession → sessID
  └─ loop = newLoop(ctx, system, sessID)
        └─ mgr.Ensure(sessID) → Handle
        └─ reg = {read_file, write_file, list_dir}(sandbox, Handle)
        └─ loop.WithTools(reg)
  └─ registry.Submit → Loop.Run
        iter 1: toolDefs() → provider sees read_file/write_file/list_dir schemas
                model emits tool_use(read_file, {path})
                dispatch → CallAll → LocalPort.ReadFile(resolve(path))
                tool_result appended → produced
        iter 2: model answers with text → KindDone
```

## Risks

- **Symlink escape** on the local backend — mitigated by D3's EvalSymlinks
  containment check (unit-tested with a planted symlink).
- **Local backend is weak isolation** (host fs, shared kernel) — acceptable for
  dev/self-host single-tenant; multi-tenant deploys should use `docker`. This is
  documented on the config and is the same trust note as the Docker backend's
  egress TODO.
- **No solidify on local writes** — the live dir is the state; versioned
  snapshots stay a workspace concern (non-goal, restated).
