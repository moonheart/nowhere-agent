# context-compression — tasks

## 1. Round-based splitting + pairing repair (contextmgmt)

- [ ] 1.1 `groupRounds(view []provider.Message) []round` — group into rounds (assistant + its tool_result answers); a text-only turn is a single-message round.
- [ ] 1.2 Rework `Compress` to drop **whole rounds** (replace the `KeepRecent` message-count split with keep-recent-N-rounds); keep `Policy`, `Threshold`, and the `Compressor` interface stable.
- [ ] 1.3 `EnsurePairing(view) []provider.Message` — strip orphan tool_result (placeholder if a message empties), synthesize `is_error` tool_result for dangling tool_use, dedupe ids.
- [ ] 1.4 Tests: round grouping never severs a tool_use/tool_result pair; Compress keeps recent rounds verbatim; EnsurePairing fixes orphan/dangling/duplicate.

## 2. LLM compressor (contextmgmt)

- [ ] 2.1 `LLMCompressor{adapter, model}` implements `Compressor`: one streaming call with `Tools: nil` over the shared adapter, structured summary prompt, honours ctx (cancel aborts the summarize).
- [ ] 2.2 Structured summary prompt (condensed CC template: intent / key concepts / files / errors / current state / next step); `<analysis>`-style scratch stripped if present.
- [ ] 2.3 Tests with a fake adapter: emits `Tools: nil`, summarizes the dropped rounds, honours cancellation, propagates adapter error.

## 3. Working view + in-loop compression (agent)

- [ ] 3.1 Loop maintains a working view (history + produced) separate from the durable record; compression rewrites only the view.
- [ ] 3.2 Per-iteration, before building the request: `EnsurePairing`, then compress the view when over budget.
- [ ] 3.3 `agent.Config` gains `ContextWindow` (per model) + `Compressor` + `MaxCompressFailures`; budget = `window − maxOutput`, trigger at `Policy.Threshold`.
- [ ] 3.4 Circuit breaker: after N consecutive compress failures, stop compressing for the run.
- [ ] 3.5 Tests: view compressed over threshold while durable-history input is untouched; pairing repaired before send; breaker trips after N failures.

## 4. Reactive context-overflow fallback (agent + provider)

- [ ] 4.1 Adapters surface context-overflow (413 / prompt_too_long) as a recognisable error.
- [ ] 4.2 Loop: on overflow, drop oldest round(s) and retry, bounded; distinguish overflow from a real failure.
- [ ] 4.3 Tests: overflow triggers drop+retry; non-overflow error fails the run.

## 5. Wiring + verify

- [ ] 5.1 cmd/server: build `LLMCompressor` over the configured adapter; set `ContextWindow` per model; pass into the loop factory.
- [ ] 5.2 `go test ./...` green.
- [ ] 5.3 `openspec validate context-compression --strict` clean; archive.
