# Tasks: decouple-run-ownership

## 1. EventBus port + in-memory implementation
- [x] 1.1 Define `EventBus` interface (`Publish`/`Subscribe`) in `internal/session`
- [x] 1.2 Implement in-memory bus (extract today's `Runtime.Subscribe` fan-out); unit tests for fan-out, unsubscribe, slow-consumer drop

## 2. RunRegistry (run ownership off the HTTP request)
- [x] 2.1 `RunRegistry`: `Submit(ctx, sessionID, loopFactory) (runID, error)`, goroutine-per-run, owns run context + cancel; single-active-run preserved
- [x] 2.2 `CancelRun` via registry (transport-independent); unit tests incl. cancel-before-start, cancel-during-run
- [x] 2.3 Worker publishes terminal event (persist → publish → settle) before `CompleteRun`; regression test for the attach-side settle race

## 3. Unified attach path in chatapi
- [x] 3.1 Extract `attach(w, sessionID, after)` helper (subscribe → replay gap → live-follow → terminal replay-fill); shared by serveChat and serveResume
- [x] 3.2 serveChat = Submit + attach(after=0); HTTP/SSE contract unchanged
- [x] 3.3 serveResume = attach(after=N); drop the privileged-submitter stream path
- [x] 3.4 serveCancel targets the registry; works for submitter and attacher alike

## 4. Frontend symmetry
- [x] 4.1 Attacher Stop calls `POST /api/chat/cancel` (same as submitter); remove the `onCancel`-only dependency
- [x] 4.2 Simplify App.tsx polling/attach where symmetric attach covers it (no behavioural regression on multi-tab)

## 5. Verification
- [x] 5.1 Unit tests green (`go test ./...`), incl. new registry/bus/attach tests
- [x] 5.2 E2E vs mockllm: single-tab cancel, two-tab cancel-from-tab2, attach-mid-run single message, submitter-disconnect run survives
- [x] 5.3 `openspec validate decouple-run-ownership` passes
