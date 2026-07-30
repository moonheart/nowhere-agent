# generic-interrupt — design

## Model: Interaction = suspend with payload, resume with value

A general interrupt is three things: (1) suspend carrying an arbitrary payload,
(2) resume carrying an arbitrary value, (3) a handler that folds the value back
into the conversation. The existing `Approval` record already has all three
(`tool_input`/`kind`, `answer`, and `Decide`'s fold). We generalize the names
and make the fold pluggable.

```go
package session

// Interaction is the durable record of a run suspended waiting on the client
// (general interrupt). Kind is an OPEN string interpreted by a registered
// InteractionHandler. Payload is what the client is shown (was tool_input);
// Result is what the client returns (was answer, generalized).
type Interaction struct {
    ID        string
    RunID     string
    SessionID string
    ToolCallID string
    Kind      string          // "tool_approval" | "ask_user" | "client_tool" | ...
    ToolName  string
    Payload   json.RawMessage // shown to the client
    Result    json.RawMessage // returned by the client (nil until resolved)
    Status    InteractionStatus
    CreatedAt time.Time
    DecidedAt *time.Time
}
```

`Approval` becomes a type alias / rename of `Interaction` (kept source-compat
where convenient); the table reuses the existing `approvals` columns — `kind`,
`tool_input` (→ Payload), `answer` (→ Result) — via a rename/alias migration, so
existing rows stay valid.

## InteractionHandler: the pluggable fold

The per-kind knowledge (how to fold the client's result into a `tool_result`)
moves out of `Decide`'s switch into a registered handler:

```go
package session

// InteractionHandler folds a resolved Interaction into the tool_result a fresh
// run appends. One handler per Kind; registered on the RunRegistry.
type InteractionHandler interface {
    // Fold turns the client's Result into the tool_result for the suspended
    // call. tools is the session-bound registry (needed when a handler must
    // execute an approved call); it may be nil for handlers that never execute.
    Fold(ctx context.Context, in Interaction, tools *toolruntime.Registry) (toolruntime.Result, error)
}
```

`RunRegistry` gains `RegisterInteractionHandler(kind string, h InteractionHandler)`
and a default map wiring the three built-in kinds:

- **`tool_approval`** — `Result{decision}`: approve → execute the gated call via
  `tools`, fold its real result; reject → an is_error denial. (The current
  permission branch.)
- **`ask_user`** — `Result{answers}`: fold the structured answers as the
  tool_result content; skip (no result) → a "skipped" note. (The current
  ask_user branch.)
- **`client_tool`** — `Result{output}` or `Result{error}`: validate `output`
  against the tool's declared output schema; valid → fold as the tool_result;
  invalid/error → an is_error result so the model self-corrects. Never executes
  server-side.

`Decide` shrinks to: resolve the row → look up `handlerByKind[ap.Kind]` →
`handler.Fold(...)` → append the returned `tool_result` to the rebuilt history.
No per-kind `switch`.

## Unified suspend point in the loop

`interactionGate` collapses from two special-case branches to one check: a tool
call interrupts the run when ANY of these holds —

1. it is gated for approval (`gateInteraction` returns the approval marker), or
2. it is `ask_user` (the built-in structured-question tool), or
3. it is a client-side tool.

Client-side is detected without touching the base `Tool` contract, via an
optional interface:

```go
// ClientTool is a tool that executes in the client, not on the server. The loop
// suspends on it (like an approval) and the client returns the result.
type ClientTool interface {
    toolruntime.Tool
    ClientSide() bool // true → suspend and hand the call to the client
}
```

The gate emits a single `KindInterrupt` frame (generalizing
`KindApprovalRequest`) carrying `{interactionId, kind, toolCallId, toolName,
payload}`, sets `PendingInteraction`, and ends the run cleanly — the identical
run-stateless semantics as today (the assistant message with the interrupting
tool_use is already persisted; a later run applies the result).

`KindApprovalRequest` is kept as an alias of `KindInterrupt` for back-compat
during the transition; the SSE emitter maps both to the `data-interaction`
frame.

## Client-side tool: the trust decision

The server does not execute a client-side tool, so it must decide how much to
trust the client-returned output. **Chosen: declare + validate.** A client-side
tool declares an output schema; before folding, the `client_tool` handler
validates the client's `output` against it. A mismatch (or a client-reported
`error`) becomes an is_error tool_result — the same "malformed → error → model
self-corrects" pattern the loop already uses for bad tool args. This avoids
blindly trusting client output while staying simple (no signature/trust
negotiation).

`ClientTool` therefore also exposes `OutputSchema() map[string]any` (optional;
nil → accept any output).

## Front-end

- The transient frame generalizes: `data-tool-approval` → `data-interaction`
  carrying `{interactionId, kind, toolCallId, toolName, payload}`.
- `reportApproval` → `reportInteraction`; the client renders per kind:
  - `tool_approval` → the existing approve/deny card;
  - `ask_user` → the existing question card;
  - `client_tool` → an auto-executing card: the client runs the named frontend
    capability with `payload` as input and POSTs `{output}`/`{error}` as the
    result (no human click needed unless the capability itself prompts).
- The response endpoint is unchanged (`POST /api/chat` with an `approval`
  verdict); the body generalizes to carry the interaction id + result.

## Migration (000015)

- Rename/alias `approvals.answer` semantics to `result` (keep the column; the
  Go struct field renames). Widen `kind` usage to admit `client_tool`. No data
  loss: existing `approval`/`ask_user` rows map unchanged.
- The per-session one-pending unique index is unchanged (still one outstanding
  interaction per session).

## What is deliberately NOT changed

- **Run-stateless resume**: resume still starts a fresh run from the durable
  record; this change does not add arbitrary-point suspend/resume.
- **One pending interaction per session**: batch approval stays out of scope.
- **`ask_user` suspend semantics**: identical; only its *fold* moves into a
  handler.

## Migration path (no big-bang)

1. Introduce `Interaction` (alias of `Approval`) + `InteractionHandler` registry
   with the two existing kinds; `Decide` delegates. Behavior identical.
2. Add the loop's unified interrupt check (approval + ask_user via the same
   path they use today). Behavior identical.
3. Add `ClientTool` interface + `client_tool` handler + front-end auto-exec.
   New capability, no change to existing kinds.
