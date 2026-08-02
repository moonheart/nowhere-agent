# memory-consolidation — tasks

## 1. memory: in-place update

- [x] 1.1 `port.go`: add `Update(ctx, id, content string) error` to `Port`; document that it clears the embedding
- [x] 1.2 `pgport.go`: implement `Update` — `SET content=$2, embedding=NULL, updated_at=now()`; zero rows affected → `ErrNotFound`; malformed id → `ErrNotFound` via `identity.IsMalformedID`
- [x] 1.3 `mem.go`: implement `Update` on `MemPort` with the same semantics
- [x] 1.4 `pgport.go`: add `PurgeDeprecated(ctx, before time.Time) (int, error)`
- [x] 1.5 `mem.go`: implement `PurgeDeprecated`
- [x] 1.6 Tests: update changes content and `updated_at` but not `id`/`created_at`; update clears a populated embedding; update of an absent id and of a malformed id both return `ErrNotFound`; purge deletes only deprecated rows older than the cutoff

## 2. config

- [x] 2.1 `Dreaming` gains `MaxFacts` (`DREAMING_MAX_FACTS`, default 80), `MaxInsights` (`DREAMING_MAX_INSIGHTS`, default 30), `MaxSummaries` (`DREAMING_MAX_SUMMARIES`, default 40)
- [x] 2.2 `Dreaming` gains `PurgeAfter` (`DREAMING_PURGE_AFTER`, default `720h`)
- [x] 2.3 Remove `DREAMING_REFLECT` / `DREAMING_REVISE` — the stages they gated no longer exist as separate stages
- [x] 2.4 Document all of it in `.env.example`
- [x] 2.5 Tests: defaults, overrides, and that a zero/negative cap is rejected rather than silently meaning "unbounded"

## 3. dreaming: the consolidate stage

- [x] 3.1 `pipeline.go`: `consolidateSchema` — `update[{id,content}]`, `add[{kind,content}]`, `remove[{id,reason}]`, all required, `additionalProperties:false`
- [x] 3.2 `pipeline.go`: `consolidatePrompt(newFacts, summary, existing []handledMemory, caps, today)` — renders memories as `M1: <content>` grouped by kind, states each cap and current count, states that memories describe the USER and not the assistant/conversation/memory system
- [x] 3.3 `pipeline.go`: delete `reflectSchema`/`reflectPrompt`/`reviseSchema`/`revisePrompt`/`contradicts` and their tests
- [x] 3.4 `worker.go`: `handles(mems)` builds the `M1…Mn` ↔ uuid map; resolution of an unknown handle returns not-found and is logged, never approximated
- [x] 3.5 `worker.go`: `consolidate` — one `CompleteJSON` call, then apply ops in order update → add → remove
- [x] 3.6 `worker.go`: delete `reorganize`, `reflect`, and `deprecateMatching` (the substring fallback goes with them)
- [x] 3.7 `worker.go`: `enforceCaps(ctx, scope)` — per kind, deprecate oldest live until under cap; returns what it evicted for logging
- [x] 3.8 `worker.go`: log an anomaly when one pass removes more than half a scope's live memories
- [x] 3.9 Tests: update op rewrites the named memory; add op stores; remove op deprecates; unknown handle is ignored and the rest of the ops still apply; merge (update + remove) leaves exactly one live memory; ops apply in an order where a partial failure never leaves the source removed without the target updated

## 4. dreaming: caps, budget, watermark

- [x] 4.1 `worker.go`: `processSession` becomes extract → compress → consolidate → enforceCaps
- [x] 4.2 Each stage checks the remaining allowance before spending; make the `budget` parameters load-bearing or delete them — the current state (present, unread) is the defect
- [x] 4.3 `Run`: advance the dreamed watermark only when consolidation ran; a batch skipped for budget keeps its watermark
- [x] 4.4 `Run`: purge deprecated memories older than `PurgeAfter` once per pass
- [x] 4.5 Tests: a kind over cap is evicted oldest-first; a kind at cap does not evict another kind; deprecated rows do not count toward the cap; a stage past budget is not called at all (fake LLM asserts call count); a skipped consolidation leaves the watermark unchanged and the next pass re-reads the same episodes; purge runs once per pass

## 5. Existing data

- [x] 5.1 `migrations/000017_purge_runaway_insights.up.sql`: `DELETE FROM memories WHERE kind = 'insight'`
- [x] 5.2 `migrations/000017_purge_runaway_insights.down.sql`: documented no-op — deleted rows cannot be restored
- [x] 5.3 Confirm against the dev database that facts and summaries survive and the live set lands inside the caps

## 6. Manual consolidation trigger

- [x] 6.1 `session`: `PGStore.ListUndreamedSessionsForUser`; MemStore reports it unsupported like its sibling
- [x] 6.2 `dreaming`: `EpisodeSource.PendingSessionsForUser`, `StoreSource` implementation, `Worker.RunForUser` over a shared `runOver`
- [x] 6.3 `dreaming/runner.go`: `Runner` with a process-wide single-flight lock covering scheduled AND manual passes, background execution off the root context, per-account last-run records, `Wait` for shutdown/tests
- [x] 6.4 `adminapi`: `POST /api/me/dream` (202 / 409 busy / 503 unwired), `GET /api/me/dream`; account taken from the authenticated context, never a parameter
- [x] 6.5 `cmd/server`: build the worker regardless of `DREAMING_ENABLED` so manual consolidation survives the schedule being off; scheduler now calls `Runner.RunScheduled`
- [x] 6.6 Frontend: "Consolidate now" on My memories, polling while running, last-run summary, control hidden on 503
- [x] 6.7 Frontend: `MemoryTable` hides superseded memories by default with a count and a show/hide toggle
- [x] 6.8 Tests: runner single-flight across users and against the scheduler, failure releases the lock, per-caller status; HTTP scoping/409/503; `RunForUser` touches only the caller's sessions

## 7. Compaction and fidelity (found by live testing)

- [x] 7.1 `Worker.RunForUser` falls back to a COMPACTION pass when the account has no unconsolidated sessions — otherwise "Consolidate now" is a no-op exactly when a user reaches for it
- [x] 7.2 `consolidatePrompt` swaps its framing when there is no new material: review the store itself, and call out that the same fact in two languages is one fact
- [x] 7.3 `Result.Compacted` so "nothing to do" and "no new conversations, so we tidied what was there" are distinguishable; surfaced through the API and the console
- [x] 7.4 Compaction skipped when sessions were consolidated (the store was already reviewed) and when the store is empty (no model call)
- [x] 7.5 FIDELITY rule in the prompt: never introduce a name/date/place/event absent from the sources, never invent a "the user corrected this" event. Added after a live pass merged three memories that all said the cat was 豆豆 into one claiming 欢欢, with a fabricated correction event to explain it
- [x] 7.6 Tests for all of the above, including that the fidelity rule actually reaches the model

## 8. Verification

- [x] 8.1 `go build ./...` and `go test ./...` green; `go test -race ./internal/dreaming/` clean
- [x] 8.2 `openspec validate --change memory-consolidation`
- [x] 8.3 Live pass against the real model in an isolated scratch scope: 3 LLM calls total (extract+compress+consolidate, not one per fact); a time-stale fact was revised IN PLACE rather than duplicated; two worthless near-duplicate summaries were retired; a new preference was added with absolute time; no self-referential insight was produced; the watermark advanced exactly once
- [x] 8.4 Measured: the live pass cost 4,937 tokens over 3 calls. Not directly comparable to the old pipeline's ~110k, which was measured against a 311-memory store — that is the point, since the old cost scaled with the store and the new one is bounded by the caps
- [x] 8.5 Browser pass on the console: superseded hidden by default with a working toggle; "Consolidate now" ran a real pass that revised two facts in place (0 added, 2 revised, 3.5k tokens) and refreshed the list when it finished
- [x] 8.6 Live compaction against a real 54-memory store: 2 added, 4 revised, 37 retired, 11.4k tokens, 63s. Duplicates merged correctly; the over-half-retired anomaly warning fired as designed. One merge hallucinated (see 7.5) — reported to the user, prompt hardened, data corrected by hand
