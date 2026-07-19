# context-compression

## Why

The agent loop's short-term memory grows without bound. Each iteration sends
`history + produced` verbatim to the provider, so tokens only ever increase
(`internal/agent/loop.go:114`). A long session eventually exceeds the model's
context window and the run fails — or would, if the window were ever reached.
Today there is no in-loop management at all: `internal/contextmgmt` ships a
`Compress` sliding-window helper (tasks 12.1–12.3) but **nothing calls it**
— the compressor is orphaned.

Two prerequisites landed since the helper was written, and they change what
"compress" must do:

- **persist-raw-messages** made the authoritative conversation record a
  full-block `messages` table (text, thinking+signature, tool_use, tool_result).
- **redis-stream-live** made history rebuild fast (no per-token DB).

So the raw material for a faithful compressor now exists, but the existing
helper is unsafe to use on it: `Compress` splits by **message count**
(`KeepRecent`), which can sever a `tool_use` from its `tool_result`, and it has
**no LLM summarizer** — the `Compressor` interface has no real implementation.

This change wires compression into the loop, correctly.

## What Changes

- **Working view vs durable history separation (P0).** The loop keeps two
  things: the authoritative full history (the `messages` table — never
  rewritten) and a **working view** (the message list actually sent to the
  model). Compression only ever rewrites the working view. Replay/resume and
  dreaming read the durable record, so they are unaffected by compression.
- **Round-based splitting (P0).** History is grouped into conversation rounds
  (an assistant message plus the tool_results that answer its tool_use blocks).
  Compression drops **whole rounds**, never a message count, so a tool_use is
  never separated from its result.
- **`EnsurePairing` repair (P0).** Before every provider send, the view is
  repaired: orphan `tool_result` (no matching use) is stripped, a dangling
  `tool_use` (no matching result) gets a synthesized `is_error` result, and
  duplicate ids are deduplicated. This is independent of compression — cancel
  and truncation can also leave unpaired calls.
- **LLM compressor (P0).** A real `Compressor` that makes a **single no-tools
  call** over the same `provider.Adapter` and model, with a structured summary
  prompt (intent / key concepts / files / errors / current state). The
  heuristic remains only as a fallback.
- **In-loop trigger + per-model budget (P1).** The loop compresses the working
  view when it crosses a fraction of the model's context window (reserving
  output space), not a flat token count.
- **Circuit breaker (P1).** After N consecutive compression failures, the loop
  stops trying to compress for the rest of the run rather than burning tokens.
- **Reactive context-overflow fallback (P1).** When the provider rejects a
  request as too large (context-overflow / 413), the loop drops the oldest
  round(s) and retries a bounded number of times instead of failing the run.

## Capabilities

### New Capabilities

_(none — this extends existing capabilities)_

### Modified Capabilities

- **agent-loop** — flesh out the existing open-ended "Short-term memory is
  in-context / Window management" requirement with concrete behaviour: working
  view, round-based compression trigger, pairing repair, overflow fallback.
  Existing scenario names are preserved.
- **context-management** — add requirements for round-based splitting, pairing
  repair, and the LLM summarizer (the existing "Compression may use an LLM"
  requirement is fleshed out, not replaced).

## Impact

- `internal/contextmgmt/` — round splitter, `EnsurePairing`, `LLMCompressor`,
  reworked `Compress` (round-based), per-model budget helper. Tests.
- `internal/agent/loop.go` — maintain + compress the working view; call
  `EnsurePairing` before send; reactive overflow retry; circuit breaker. Tests.
- `internal/agent/` — `Config` gains context-budget fields.
- `cmd/server/main.go` — wire the per-model budget and the LLM compressor into
  the loop factory.
- **No changes** to the messages table, run_events, the broker, replay/resume,
  or dreaming — compression is a working-view-only concern.

## Non-goals

- **Partial / selective compression** ("compress from message N") — deferred
  until there is a user-facing need.
- **Post-compression re-injection** of files/plans/skills — deferred until
  sandbox/tools land; the summary already carries the essentials.
- **Cache-sharing fork** (Claude Code's single-process optimisation) — we make
  a plain no-tools call over the shared adapter; no fork machinery.
- **Prompt-cache-aware compression placement** — compression rewrites the view
  prefix, which busts the cache for that turn; acceptable, and noted.
