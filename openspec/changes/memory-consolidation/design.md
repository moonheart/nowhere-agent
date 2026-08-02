# memory-consolidation — design

## D1. One consolidation call, not one per fact

Today's write path is four stages: `extract` (transcript → facts), `compress` (transcript →
summary), `reorganize` (per fact: full memory set → deprecations + a rewrite), `reflect` (summary
+ full memory set → insights + deprecations).

`reorganize` and `reflect` both exist to answer the same question — *given everything already
known, what should the store look like now?* — and both answer it with a partial view and a
write-only vocabulary. They merge into one **consolidate** stage:

```
episode batch ──> extract  ──┐
                             ├──> consolidate(new material + FULL live set) ──> ops
              ──> compress ──┘
```

`extract` and `compress` stay: they read the transcript, which consolidate should not have to
re-read alongside the whole store. They are also the cheap stages — their prompts are bounded by
the batch, not by history.

Cost, at today's data:

| | LLM calls / session | tokens (4 facts, 87KB live) |
|---|---|---|
| now | `2 + F + 1` = 7 | ~110k, growing with history |
| after | `2 + 1` = 3 | ~15k, bounded by the caps |

The second column is the point. Under caps the live set cannot exceed ~150 memories ≈ 42KB ≈ 11k
tokens, so a pass costs roughly the same in a year as it does today.

## D2. Edit operations, addressed by handle

The consolidate stage returns:

```json
{
  "update": [{"id": "M12", "content": "..."}],
  "add":    [{"kind": "fact", "content": "..."}],
  "remove": [{"id": "M31", "reason": "merged into M12"}]
}
```

**Why handles (`M1…Mn`) and not uuids.** A uuid is 36 characters; 150 of them is 5.4KB of prompt
spent on random strings the model has to copy exactly. Handles are short, and a transposition
produces an *unknown* handle rather than a *valid other* one. The mapping lives in Go; an
unrecognized handle is ignored and logged, never guessed at.

**Why not keep string matching.** `deprecateMatching` currently falls back to
`strings.Contains(existing, returned)` with no length floor. If the model returns a short
paraphrase, the first memory that happens to contain that substring is retired — an arbitrary,
silent, wrong deletion. Handles remove the guess entirely.

**`remove` deprecates, it does not delete.** The row stays, excluded from recall, until the purge
window closes (D5). Consolidation is a lossy judgement made by a model; a reversible one is worth
the disk.

## D3. In-place update invalidates the embedding

`Port.Update(ctx, id, content)` rewrites content, bumps `updated_at`, and **sets `embedding` to
NULL**. An embedding describes the text it was generated from; leaving it attached to rewritten
content would make `RecallVector` rank the memory by what it used to say.

Today every row has a NULL embedding (426 rows, 0 populated) because the worker has no embedder
wired, so this is preventive rather than a live bug — but the invariant belongs in the port, not
in the future caller who wires an embedder and has no reason to suspect it.

Update does not preserve prior content. A memory is a distillation, not an audit log, and
`remove` already keeps a record for the destructive direction.

## D4. Caps: model merges, machine enforces

| kind | cap | why |
|---|---|---|
| fact + preference | 80 | shared pool — both are "things true about the user", and a split would force an arbitrary boundary between "prefers X" and "is X" |
| insight | 30 | the runaway kind; a low ceiling is the actual fix |
| summary | 40 | ~40 recallable past conversations is a useful horizon |

Two stages, in order:

1. **Model.** The prompt states each cap and the current count, and instructs it to merge or
   remove so the result fits. This is the good path: merging *"asked about the tool list (7/29)"*
   and *"asked about the tool list (7/31)"* into one dated memory preserves information that
   eviction would drop.
2. **Machine.** After ops are applied, any kind still over cap is deprecated oldest-first until
   under. The model is asked to satisfy the invariant; it is not trusted to.

Caps count **live** memories only — deprecated rows are already invisible to recall, and counting
them would make the cap tighten as a side effect of normal supersession.

Chosen over a single per-scope total: with one pool, the kind that generates most freely wins the
whole budget. That is precisely the observed failure — insights at 83% of a store where facts are
the part with actual value.

## D5. Purge

Deprecated memories older than `DREAMING_PURGE_AFTER` (default 30 days) are `Forget`ed. Without
this the table grows without bound in the one dimension caps do not cover; today it holds 115
deprecated rows that nothing will ever read.

## D6. Budget enforcement and watermark safety

Each stage checks the remaining allowance before spending it, so the `budget` parameters
threaded through the pipeline finally mean something. The subtle part is what happens on a skip.

`Run` currently advances the dreamed watermark after `processSession` unconditionally. If
consolidation is skipped for budget, advancing would mark the batch consumed and **the episode
would never be learned from** — a silent, permanent loss. So: the watermark advances only if
consolidation ran.

The livelock risk this introduces (a session too expensive to ever consolidate blocks its own
watermark forever) is bounded by D4: with caps, the consolidate prompt is ~11k tokens against a
100k per-pass budget, so a single batch cannot approach it. The budget is per-pass, so a batch
deferred at the end of one pass is retried at the start of the next. Worth stating because
without caps this trade would be unsafe.

## D7. Memories are about the user

The store's current content is the argument:

> "The consolidation function's continued receipt of verbatim duplicates it has repeatedly
> diagnosed, with zero b…"

This is a true observation and a worthless memory. Nothing in the prompts ever said the subject
of a memory must be the user — `extractPrompt` says it for facts ("about the USER … NOT about
this conversation"), `reflectPrompt` does not, and reflection is the stage that ran away.

Consolidation states it for every kind: memories describe the user — who they are, what they
prefer, own, or are working on. Not the assistant, not the conversation as an artifact, not the
memory system. This is a content rule, so it is enforced by prompt; the caps are the backstop for
when it is disobeyed.

## D8. Existing data

270 insights are deleted; 51 facts and 105 summaries are kept. The insights were produced by the
loop this change removes and describe it, so there is nothing to migrate them toward.

Delivered as migration `000017` rather than a one-off script: every deployment running this code
has the same polluted store by construction, so the cleanup belongs with the code that fixes the
cause. **The down migration cannot restore deleted rows and is a documented no-op** — that is a
real cost of this choice and the reason it is called out here rather than discovered later.

## D9. What this change does not do

- **No embedder.** Vector recall stays dormant; `RecallVector` exists and nothing populates
  embeddings. Related, deliberately separate.
- **No team-scope consolidation.** Dreaming writes user scope only today. Team memory has no
  writer, so caps for it would be untested speculation.
- **No cross-session merge trigger.** Consolidation runs per session batch, as now. A periodic
  whole-store compaction pass is a plausible follow-up once caps prove they hold.

## Risks

| risk | mitigation |
|---|---|
| Model returns handles that do not exist | Unknown handles are ignored and logged; ops are applied best-effort, never guessed |
| Model empties the store via `remove` | Removes are deprecations, reversible until the purge window; a pass removing more than half the live set is logged as an anomaly |
| Caps discard something valuable | Eviction is oldest-first among live memories of one kind, and only after the model was given the chance to merge instead |
| One consolidate call is a bigger blast radius than four small ones | Ops are applied in a transaction-free best-effort order (update → add → remove) so a partial failure leaves the store consistent, never half-merged with the source already gone |
| Insight cap of 30 is too low for a real multi-project user | Configurable; 30 is calibrated against a store where 257 insights carried no information |
