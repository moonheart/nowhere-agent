# generic-interrupt — proposal

## Why

The loop today has **two special-cased human-interaction signals**, not a general
interrupt primitive:

- **Permission approval**: `interactionGate` (`loop.go`) pattern-matches a deny
  reason carrying the `ApprovalReasonPrefix` marker, sets `PendingApproval`, ends
  the run.
- **ask_user**: a separate hard-coded branch in the same `interactionGate`,
  special-cased on the tool name.

Both funnel into the same `approvals` table and the same `registry.Decide`
resume path, which already behaves like a general interrupt: a `kind` column
(`approval`/`ask_user`), a `tool_input` payload shown to the client, an `answer`
result the client returns, and a per-session "one pending interaction" rule. The
durable record is 80% general already — but the *suspend point* (loop) and the
*fold logic* (`Decide`'s `switch`) are closed enumerations of the two kinds.

We now want a **third kind that does not fit either branch**: a **client-side
tool** — a tool the model calls that must execute in the *client* (browser),
not on the server (read clipboard, surface a native picker, drive a frontend
capability). The server cannot run it; it must suspend, hand the call to the
client, and fold the client-returned output back as the tool result. This is
isomorphic to ask_user (server sends a payload, client returns a result) but is
triggered by "the model called a tool marked client-side", not by the model
calling `ask_user`.

Adding a third `if` to `interactionGate` and a third `case` to `Decide` would
cement the wrong shape. The right move is the LangGraph insight: **`interrupt`
is a general primitive** — suspend carrying an arbitrary payload, resume
carrying an arbitrary value, and a registered handler knows how to fold that
value back. This change makes the loop's interrupt general.

## What changes

- **`Approval` → `Interaction`**: generalize the durable record. `kind` becomes
  an open string interpreted by a registered handler; `answer` generalizes to
  `result` (any client-returned JSON). Existing `approval`/`ask_user` rows map
  onto the same shape (backward compatible via migration).
- **A registry of interaction handlers**: `Decide`'s inline
  `switch isAskUser/approve` becomes a per-kind `InteractionHandler` with one
  method — fold the client result into the `tool_result` a fresh run appends.
  New interaction kinds are added by registering a handler, not by editing the
  resume path.
- **A unified suspend point in the loop**: one interrupt check replaces the two
  special-case branches. A tool call suspends the run when it is (a) gated for
  approval, (b) `ask_user`, or (c) a client-side tool. Client-side tools are
  identified by an optional interface (`interface{ ClientSide() bool }`) so the
  base `Tool` contract is untouched.
- **Client-side tool kind**: server does not execute it; it suspends, streams a
  `data-interaction` frame, the client executes and returns `{output}` or
  `{error}`, which is folded back as the tool result. Client output is validated
  against the tool's declared output schema before folding (reusing the existing
  "malformed → error result → model self-corrects" pattern).
- **Front-end**: the `data-tool-approval` frame generalizes to
  `data-interaction`; the client renders per kind (approval card / question
  card / auto-executing client-tool card).

## Out of scope

- **Batch approval** (multiple gated calls in one model turn): the loop still
  suspends on the first interrupting call. Generalizing the primitive does not
  change the one-pending-per-session rule. (Tracked separately.)
- **A general graph `interrupt()` that suspends at arbitrary points**: we remain
  run-stateless; resume always starts a fresh run from the durable record. This
  change generalizes *what* can suspend a run, not *where* resume continues from.
- **edit decisions** (approve-with-modified-args): the `InteractionHandler` shape
  leaves room for it, but no edit UI is built here.

## Success criteria

- The loop's suspend logic is a single interrupt check with no per-kind `if`s;
  `ask_user` and approval are just two registered kinds.
- `registry.Decide` has no per-kind `switch`; it delegates to the kind's
  registered `InteractionHandler`.
- A client-side tool call suspends the run, the client executes it, and its
  validated output is fed back as the tool result on resume.
- `go test ./...` green, including a new client-tool end-to-end test.
