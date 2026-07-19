# context-compression — design

> Source analysis: `docs/claude-code-comparison/context-management.md` (Claude
> Code `services/compact` mechanisms, with file:line citations). This file turns
> that into nowhere-specific decisions. Nothing here copies CC wholesale; each
> decision notes what we take and what we deliberately don't.

## Context

The loop sends `append(history, produced...)` to the provider every iteration
(`loop.go:114`). Nothing bounds it. `internal/contextmgmt` has a `Compress`
helper that (a) is never called, (b) splits by message count, and (c) has no
real summarizer. Meanwhile the durable record is now a full-block `messages`
table (persist-raw-messages) and history rebuild is fast (redis-stream-live).
The stage is set; the loop is the missing actor.

The core problem compression must solve here that it didn't before: **the
history is no longer flat text.** It contains `tool_use`/`tool_result` pairs
with a hard API contract (every `tool_use` must be answered by a `tool_result`
in the immediately following message). Any naive cut breaks that contract and
the provider rejects the request.

## Decisions

### D1 — Working view vs durable history (P0)

The loop maintains a **working view**: the `[]provider.Message` actually sent to
the model. Compression rewrites only this view. The durable `messages` table is
never touched by compression — replay/resume (`serveHistory`) and dreaming read
the full record, so a compressed run still replays in full and dreams over
complete episodes.

```
durable history (messages table) ──► replay/resume, dreaming   [never rewritten]
        │ rebuild at run start
        ▼
   working view (in-loop) ──► provider each iteration           [compress rewrites]
```

This mirrors CC's `messagesForQuery` replacement + append-only transcript, but
expressed as "view" rather than CC's boundary-marker + transcript-append. We
don't need a persisted boundary marker because our working view is rebuilt from
the durable store at each run start — it never has to survive a restart
mid-run.

**Why not compress the durable record:** that would corrupt replay and dreaming
(the dreaming worker is supposed to recover detail that compression dropped —
see the existing "Separation from dreaming" requirement). Compression is a
per-run, in-context concern only.

### D2 — Round-based splitting (P0)

Group the view into **rounds**. A round is one assistant message plus the
tool_result message(s) answering its tool_use blocks; a plain text turn is a
single-message round. Compression drops **whole rounds** from the front, never a
message count.

CC achieves this via `groupMessagesByApiRound` (assistant `message.id` change =
new round; the API contract guarantees pairing within a round). We don't have
provider message ids on our canonical model, so we group structurally: walk the
view, and an assistant message's round extends forward through the tool_result
messages whose `ToolResultID` matches its `ToolUseID`s.

**Replaces** the `KeepRecent` message-count split. Keeping the most recent N
rounds verbatim (rather than N messages) preserves the "recent context stays
verbatim" behaviour without ever severing a pair.

### D3 — `EnsurePairing` repair before every send (P0)

Independent of compression, the view can carry unpaired blocks: a cancelled run
may have persisted a `tool_use` whose result never ran; truncation can drop a
result. Before every provider send, `EnsurePairing` normalizes the view:

- **orphan `tool_result`** (no matching `tool_use` earlier) → dropped; if that
  empties a message, a placeholder text block keeps the message non-empty.
- **dangling `tool_use`** (no matching `tool_result` in the next message) → a
  synthesized `tool_result{IsError: true, content: "[Tool use interrupted]"}` is
  appended.
- **duplicate ids** → deduplicated.

This is CC's `ensureToolResultPairing` (src/utils/messages.ts), run per send.
It's cheap (a map over ids) and turns a whole class of "provider rejects the
request" failures into a no-op.

### D4 — LLM compressor over the shared adapter (P0)

A real `Compressor` implementation: `LLMCompressor` makes a **single streaming
call with `Tools: nil`** over the same `provider.Adapter` and model the loop
uses, prompting for a structured summary. No-tools means the model can only
emit text — the same guarantee CC gets from its deny-all fork, without the fork.

The prompt is a condensed version of CC's 9-section template (intent, key
technical concepts, files/code, errors & fixes, current state, next step). We
drop CC's analysis/scratch preamble and the fork/cache-sharing machinery — a
plain call over the shared adapter already reuses connection + auth, and our
prefix cache benefit is minor at this scale.

**Heuristic stays only as fallback**: if the LLM compressor errors, the caller
may fall back to a truncation summary rather than fail the run (see D6).

### D5 — In-loop trigger on a per-model budget (P1)

The loop compresses the working view when its estimated size crosses a fraction
of the **model's context window**, reserving output space:

```
budget   = contextWindow − maxOutput          (room for the reply)
trigger  = estimateTokens(view) > budget·threshold
```

`contextWindow` comes from `agent.Config` (per model), defaulting sensibly;
`threshold` reuses the existing `Policy.Threshold` (0.8). This replaces the flat
`MaxTokens` budget with a window-relative one, matching CC's
`getEffectiveContextWindowSize`/`getAutoCompactThreshold` without the extra
warning/error/blocking tiers (overkill for now).

### D6 — Circuit breaker (P1)

If compression fails N consecutive times (LLM compressor error), the loop stops
attempting compression for the rest of the run and lets the reactive fallback
(D7) handle overflow. Prevents a broken compressor from adding latency + cost to
every iteration. CC uses `MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3`; same idea.

### D7 — Reactive context-overflow fallback (P1)

When the provider rejects a request as context-overflow (HTTP 413 /
`prompt_too_long` / the provider's overflow error), the loop drops the oldest
round(s) from the view and retries, up to a small bound, instead of failing the
run. This is the last-resort net when the threshold trigger mis-estimates (our
`estimateTokens` is ~4 chars/token — crude). Adapters surface overflow as a
typed/recognisable error so the loop can distinguish it from a real failure.

### D8 — Where compression runs in the loop

Compression is checked **once per iteration, before building the request** —
i.e. on `view = history + produced-so-far`. This is the only point where the
view is about to be sent and can be shrunk safely (between turns, never
mid-stream). The summarize call itself is a provider stream, so it honours the
run ctx (a cancelled run aborts an in-flight summarize).

## Shape of the change

```
internal/contextmgmt/
  rounds.go        — groupRounds(view) []round; dropOldestRounds
  pairing.go       — EnsurePairing(view) view
  compress.go      — Compress reworked to round-based (keeps Policy, Threshold)
  llm.go           — LLMCompressor{adapter, model, prompt} implements Compressor
  budget.go        — BudgetFor(model window, maxOutput, threshold)
internal/agent/
  loop.go          — working view + per-iteration compress + EnsurePairing + overflow retry + breaker
  loop.go Config   — + ContextWindow, + Compressor, + MaxCompressFailures
cmd/server/main.go — build LLMCompressor over the adapter; set ContextWindow per model
```

## What we explicitly don't take from CC

- **Fork + prompt-cache sharing** — single-process CLI optimisation; a no-tools
  call over the shared adapter is enough.
- **Boundary marker persisted to the transcript** — our view is rebuilt per run;
  nothing to mark.
- **File/plan/skill re-injection after compress** — deferred until sandbox/tools
  land; the summary carries the essentials.
- **Partial (selective) compression** — deferred until a user asks for it.
- **Multi-tier thresholds (warning/error/blocking)** — one trigger + one
  reactive net is enough at this stage.

## Risks

- **Prompt-cache bust on compress**: rewriting the view prefix invalidates the
  cache for that turn. Accepted — compression is infrequent relative to turns,
  and correctness beats cache warmth. (CC pays the same cost.)
- **`estimateTokens` is crude** (~4 chars/token, ignores per-block overhead).
  Mitigated by the reactive overflow fallback (D7), which is the real backstop.
- **Summarize cost**: one extra model call per compression. Bounded by the
  trigger threshold (compresses rarely) and the circuit breaker (D6).
