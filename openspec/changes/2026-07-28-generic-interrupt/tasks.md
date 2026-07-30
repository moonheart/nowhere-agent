# generic-interrupt — tasks

## 1. Interaction model (generalize Approval)

- [x] 1.1 In `internal/session/approval.go`, introduce `Interaction` as the durable record (rename of `Approval`): `Payload` (was `ToolInput`), `Result` (was `Answer`), `Kind` open string. Keep `Approval` as a type alias for source compatibility during the transition.
- [x] 1.2 Migration `000015_interaction.up/down.sql`: widen usage of `approvals.kind` to admit `client_tool`; add a comment documenting `tool_input`→payload, `answer`→result. No data migration (existing rows stay valid).
- [x] 1.3 Update `MemStore`/`PGStore` to the `Interaction` naming; keep the one-pending-per-session unique index and its tests passing.
- [x] 1.4 Unit tests: store round-trip for all three kinds; one-pending-per-session still enforced.

## 2. InteractionHandler registry (pluggable fold)

- [x] 2.1 Define `InteractionHandler` (`Fold(ctx, in Interaction, approve bool, tools *toolruntime.Registry) (toolruntime.Result, error)`) in `internal/session`.
- [x] 2.2 Add `RunRegistry.RegisterInteractionHandler(kind string, h InteractionHandler)` + a default map.
- [x] 2.3 Implement `tool_approval` handler: approve → execute the gated call via `tools`, fold the real result; reject → is_error denial. (Moves the current permission branch out of `Decide`.)
- [x] 2.4 Implement `ask_user` handler: `Result{answers}` → fold structured answers; skip → "skipped" note. (Moves the current ask_user branch.)
- [x] 2.5 Implement `client_tool` handler: `Result{output}`/`Result{error}`; validate `output` against the tool's declared output schema; invalid/error → is_error result so the model self-corrects. Never executes server-side.
- [x] 2.6 Rewrite `RunRegistry.Decide` to: resolve the row → look up `handlerByKind[ap.Kind]` → `handler.Fold(...)` → append the returned `tool_result` to the rebuilt history. Remove the per-kind `switch`.
- [x] 2.7 Unit tests: each handler's fold (approve/reject, answers/skip, valid/invalid/error output); `Decide` delegates with no switch; unknown kind → clear error.

## 3. Unified suspend point in the loop

- [x] 3.1 Add `agent.KindInterrupt` (generalizing `KindApprovalRequest`); keep `KindApprovalRequest` as an alias for back-compat. The SSE emitter maps both to `data-interaction`.
- [x] 3.2 Add the `ClientTool` optional interface (`toolruntime.Tool` + `ClientSide() bool` + optional `OutputSchema() map[string]any`) in `internal/toolruntime` (or `agent` — place beside `Tool`).
- [x] 3.3 Collapse `interactionGate` to one interrupt check: suspend when (a) `gateInteraction` returns the approval marker, (b) the call is `ask_user`, or (c) the tool is a `ClientTool` with `ClientSide()==true`. Emit a single `KindInterrupt` frame carrying `{interactionId, kind, toolCallId, toolName, payload}`; set `PendingInteraction` (rename of `PendingApproval`); end the run cleanly.
- [x] 3.4 Surface `PendingInteraction` on the `Loop` (rename `PendingApproval`, keep an alias during transition).
- [x] 3.5 Unit tests: one interrupt check with no per-kind `if`s (structural); each of the three triggers suspends with the right `kind`; run-stateless semantics unchanged (assistant msg with tool_use persisted, run ends nil-error).

## 4. Client-side tool wiring

- [x] 4.1 Provide a `builtin.NewClientTool(name, description, inputSchema, outputSchema)` helper producing a `ClientTool` whose `Call` is never reached (loop suspends first).
- [x] 4.2 Register client-declared tools from the chat request body (`req.Tools`) via `registerClientTools` in the chat handler — the client owns declaration (name/description/inputSchema/outputSchema), skipping any name that collides with a built-in. (Supersedes the original server-side `CLIENT_TOOLS_ENABLED` flag: client tools are registered by the client, not the server.)
- [x] 4.3 End-to-end test: model calls a client tool → run suspends with `kind=client_tool` → `Decide` folds a validated client `output` as the tool_result on resume; an invalid output → is_error result.

## 5. Front-end

- [x] 5.1 Generalize the transient frame `data-tool-approval` → `data-interaction` (`{interactionId, kind, toolCallId, toolName, args}`); keep reading the legacy `tool-approval` frame during transition (App.tsx onData + history.ts resume/follow decoders).
- [x] 5.2 Rename `reportApproval` → `reportInteraction`; render per kind: approval card, question card, auto-executing client-tool card (`client-tools.ts` runs the named browser capability with `args`, POSTs `{output}`/`{error}`). Declare the browser's client tools in the chat request body's `tools` field.
- [x] 5.3 Generalize the POST body to carry the interaction id + result (endpoint unchanged): `respondToClientTool` posts `{approved:true, answer:{output|error}}`.

## 6. Validation

- [x] 6.1 `openspec validate 2026-07-28-generic-interrupt --strict` passes.
- [x] 6.2 `go test ./...` green (23 packages).
- [x] 6.3 `go vet ./...` clean; `gofmt` clean (touched files); front-end `tsc -b`, `oxlint`, `vite build` clean.
