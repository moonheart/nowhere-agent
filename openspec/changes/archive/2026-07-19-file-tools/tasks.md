# file-tools — tasks

## 1. LocalPort sandbox backend (internal/sandbox)

- [x] 1.1 `LocalPort{root}` implementing `Port`: `Create` makes `<root>/<sessionID>` (or honours `opts.WorkspaceDir`), `Destroy` removes it, `Handle.ID = "local-<sessionID>"`.
- [x] 1.2 `resolve(path)` confinement: reject absolute paths, reject `..` escaping root, `EvalSymlinks` containment check (file and root) so a symlink inside the workspace can't point outside it.
- [x] 1.3 `ReadFile`/`WriteFile`/`ListDir` over `resolve`; `WriteFile` creates parent dirs; `ListDir` returns entry names.
- [x] 1.4 `Exec` via `os/exec` with `cmd.Dir = workspace` (interface completeness; network policy documented no-op for local backend).
- [x] 1.5 Tests: read/write/list round-trip; `..` escape rejected; symlink escape rejected; absolute path rejected; destroy removes dir; Manager works over LocalPort.

## 2. Built-in file tools (internal/toolruntime/builtin)

- [x] 2.1 `builtin.FileTools(sb sandbox.Port, h sandbox.Handle) []toolruntime.Tool` returning `read_file`, `write_file`, `list_dir` bound to the handle.
- [x] 2.2 `read_file`: args `{path}` → `Port.ReadFile`, content as string; missing/unreadable → `IsError` result with reason.
- [x] 2.3 `write_file`: args `{path, content}` → `Port.WriteFile`; reports bytes written; failure → `IsError`.
- [x] 2.4 `list_dir`: args `{path}` (default `.`) → `Port.ListDir`, newline-joined names; failure → `IsError`.
- [x] 2.5 Each tool: correct `Name/Description/Schema/Risk/Timeout` (`read_file`/`list_dir` = `RiskReadOnly`, `write_file` = `RiskSandboxWrite`).
- [x] 2.6 Tests against `sandbox.NewMemPort()`: round-trip through the real `Tool` interface; error paths produce `IsError` results; schemas are valid objects.

## 3. Loop + chatapi wiring

- [x] 3.1 `agent.Loop.WithTools(reg *toolruntime.Registry) *Loop` — post-construction registry injection (mirrors `WithImages`); nil-safe default stays empty.
- [x] 3.2 `chatapi.ToolBinder` option + `Handler.WithToolBinder`: the binder runs per run after `resolveSession`, attaching the session's sandbox-bound tools (keeps `LoopFactory` signature unchanged).
- [x] 3.3 Tests: loop drives a registered tool end-to-end (tool_use → dispatch → tool_result → final text); `WithTools` swaps the registry.

## 4. cmd/server + config

- [x] 4.1 Config: `SANDBOX_BACKEND` (`off` default | `local` | `docker`) and `SANDBOX_WORKSPACE_DIR` (fallback `WORKSPACE_DIR`).
- [x] 4.2 Build a `sandbox.Manager` in `run()` for the configured backend (`local` → `LocalPort`, `docker` → `DockerPort`); skip when `off`.
- [x] 4.3 Loop factory closure: when a manager exists and sessionID != "", `mgr.Ensure` → `builtin.FileTools` → register → `WithTools`.
- [x] 4.4 Update `.env.example` with the new vars.

## 5. Verify + archive

- [x] 5.1 `go test ./...` green (new + existing).
- [x] 5.2 `openspec validate file-tools --strict` clean.
- [x] 5.3 `openspec archive file-tools --yes`; specs synced.
