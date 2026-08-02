# memory-consolidation — proposal

## Why

Dreaming was built to consolidate memory. Measured against the running deployment, it is the
largest single producer of memory instead.

```
kind    | deprecated | count        live: 311 memories / 87,536 chars
--------+------------+------        insight share of live set: 83%
fact    |     f      |    31        deprecated, never purged: 115
insight |     f      |   257  <--
summary |     f      |    23
summary |     t      |    82
```

152 sessions have produced 270 insights — 2.6 per pass against a prompt ceiling of "at most 3",
so the reflect stage hits its cap nearly every time it runs.

**The reflect stage reads its own output.** `Worker.reflect` passes every live memory into the
prompt and asks for "higher-level patterns spanning multiple memories". Insights *are* memories,
so each pass generalizes over its own prior generalizations. The store has converged on
self-referential commentary — these are verbatim live memories:

> "With the store now dominated by near-identical duplication meta-commentary, further diagnoses
> of the pattern a…"
>
> "The consolidation function's continued receipt of verbatim duplicates it has repeatedly
> diagnosed, with zero b…"

The model is writing memories about the memory system. Nothing in the pipeline says a memory must
be about the *user*.

The deprecation machinery is not what failed: reflect retired **82 of 105** summaries, because
repeated conversations produce recognizably duplicate summaries. It retired **13 of 270**
insights, because every meta-insight is freshly worded and none is a literal duplicate. What is
missing is a **ceiling** and an input that excludes the loop — not a better prompt.

Two further defects compound it:

- **The budget is decorative.** `extract`, `compress`, `reflect`, and `reorganize` each accept a
  `budget int` parameter and none of them reads it (`worker.go:228,241,293,342`). Go does not
  flag unused parameters, so the guard looks present and does nothing. The only real check is
  between sessions in `Run`.
- **Cost is quadratic in history.** `reorganize` makes one LLM call *per extracted fact*, each
  carrying the entire live memory set. At today's 87KB (~22k tokens), a session yielding 4 facts
  costs `4×22k + 22k ≈ 110k` tokens — more than `DREAMING_MAX_TOKENS=100000` for the *whole*
  pass, spent inside a single session that is never checked mid-flight. The bill grows with the
  store, and the store grows because of the loop above.

The read path is not implicated: injection takes only fact/preference at limit 8, and
`recall_memory` caps at 20. Conversation quality is unaffected. This is a write-path defect that
burns tokens and gets more expensive every hour.

## What Changes

- **One consolidation pass replaces `reorganize` + `reflect`.** Per session batch, a single LLM
  call receives the new episode material *and the complete live memory set*, and returns edit
  operations — `update` an existing memory, `add` a new one, `remove` one that has been merged or
  superseded. Memories may now be **revised in place**, which the append-plus-deprecate model
  could not express. This is also what makes the pass affordable: one call carrying the set,
  instead of one call per fact each carrying the set.
- **Per-kind live caps**, enforced in two stages: the prompt states the caps and current counts
  and asks the model to merge to fit; after ops are applied, a mechanical oldest-first eviction
  brings any over-cap kind back under. An invariant is never left to the model alone.

  | kind | cap |
  |---|---|
  | fact + preference (shared pool) | 80 |
  | insight | 30 |
  | summary | 40 |

- **Stable handles instead of substring matching.** The prompt labels memories `M1…Mn`; the model
  addresses them by handle. Today `deprecateMatching` falls back to `strings.Contains` with no
  length floor, so a short paraphrase can retire an arbitrary unrelated memory.
- **Memories must be about the user.** Insights about the assistant, the conversation as an
  artifact, or the memory system itself are excluded by the prompt and are not storable content.
- **The budget becomes real.** Each stage checks the remaining allowance before spending, and a
  batch whose consolidation was skipped for budget does **not** advance its dreamed watermark —
  otherwise the episode is silently lost rather than retried.
- **Deprecated memories are purged** after a retention window, so the table stops growing.
- **The 270 accumulated insights are deleted** by migration. Facts and summaries are kept.

## Capabilities

- `dreaming` — MODIFIED: reorganization and reflection merge into one consolidation stage; caps,
  self-reference exclusion, real budget enforcement, watermark safety.
- `memory` — MODIFIED: the write side gains in-place update; scope-level cap and purge semantics.

## Impact

- `internal/dreaming` — `worker.go` pipeline restructured; `pipeline.go` schema/prompt rewrite.
- `internal/memory` — `Port.Update`; implementations in `pgport.go` and `mem.go`.
- `internal/config` — `Dreaming` gains cap and retention settings.
- `migrations/000017_purge_runaway_insights` — deletes existing insight rows. **Irreversible**:
  the down migration cannot restore deleted rows and is a documented no-op.
- No API or frontend change. The admin console already lists and deletes memories.
