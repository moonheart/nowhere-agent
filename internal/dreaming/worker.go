// Package dreaming implements the dreaming capability (design D6): a scheduled
// offline worker that is the ONLY writer to long-term memory. It reads
// persisted run episodes for sessions with unconsolidated messages and folds
// them into user/team-scoped long-term memory.
//
// Dreaming is INCREMENTAL (capability-gap K1, watermark model): each session
// carries a high-water mark (the messages.id consolidated up to), and the worker
// learns from the messages beyond it. A conversation therefore stays open and
// resumable while it is being learned from — learning no longer requires the
// session to end.
//
// The pipeline is EXTRACT → COMPRESS → CONSOLIDATE (memory-consolidation).
// Extract and compress read the transcript; consolidate receives their output
// as new material together with the scope's ENTIRE live memory set, and returns
// edits — revise an existing memory, add one, retire one. Consolidation is a
// single call per batch regardless of how much the batch yielded, and its
// prompt is bounded by the per-kind caps, so a pass costs about the same after a
// year as it does on day one.
//
// It replaced a per-fact revise stage plus a separate reflect stage. Those had
// two compounding defects: reflect read its own output (insights are memories,
// so it generalized over its own generalizations until 83% of the store was
// self-referential commentary), and revise ran one full-store LLM call per
// extracted fact, making cost quadratic in a history that the first defect was
// inflating.
package dreaming

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// LLM is the model the worker calls for extraction/summarization/consolidation.
// Complete is a free-text completion; CompleteJSON is a structured completion
// (capability L3) that forces a JSON object conforming to spec.Schema into out
// (a pointer). Both return the tokens consumed (for budget accounting).
type LLM interface {
	Complete(ctx context.Context, prompt string) (string, int, error)
	// CompleteJSON forces structured output: out is a pointer the JSON object is
	// unmarshalled into. Implementations without structured support may fall
	// back to parsing Complete's text.
	CompleteJSON(ctx context.Context, prompt string, spec *provider.JSONResponseSpec, out any) (int, error)
}

// Budget caps the LLM tokens a single run may spend.
type Budget struct {
	MaxTokens int
}

// Result reports what a run did (feeds observability metrics).
type Result struct {
	EpisodesProcessed int
	MemoriesWritten   int
	MemoriesRevised   int
	MemoriesRetired   int
	MemoriesPurged    int
	TokensUsed        int
	BudgetExhausted   bool
	// Compacted reports that the pass reviewed the existing store rather than
	// learning from new episodes. It distinguishes "there was nothing to do"
	// from "there were no new conversations, so we tidied what was already
	// there" — which look identical in the counters when both come back zero.
	Compacted bool
}

// EpisodeSource provides episodes (persisted conversation messages) for
// sessions that have messages the worker has not yet consolidated. Episodes
// come from the session's full-block message record (redis-stream-live D6), not
// the run event log — the message store holds the complete conversation content
// the worker consolidates.
type EpisodeSource interface {
	// PendingSessions returns sessions with messages beyond their dreamed
	// watermark (any status — open conversations are learnable), oldest first,
	// each carrying the watermark to start from.
	PendingSessions(ctx context.Context) ([]PendingSession, error)
	// PendingSessionsForUser is PendingSessions narrowed to one owner. It backs
	// user-triggered consolidation, where reading another account's sessions
	// would be both a privacy breach and a way to spend one user's request on
	// another user's tokens.
	PendingSessionsForUser(ctx context.Context, userID string) ([]PendingSession, error)
	// Episodes returns the session's persisted messages with id > afterSeq (the
	// watermark), ordered by seq — only the not-yet-dreamed tail.
	Episodes(ctx context.Context, sessionID string, afterSeq int64) ([]session.StoredMessage, error)
	// MarkProcessed advances the session's dreamed watermark to the newest
	// message the worker consumed, so the next pass starts after it.
	MarkProcessed(ctx context.Context, sessionID string, newSeq int64) error
}

// PendingSession is a session eligible for a dreaming pass: it has messages the
// worker has not yet consolidated. Seq is the watermark (messages.id) to resume
// from — 0 means nothing consolidated yet.
type PendingSession struct {
	session.Session
	Seq int64
}

// Worker consolidates episodes into long-term memory.
type Worker struct {
	episodes EpisodeSource
	memory   memory.Port
	llm      LLM
	budget   Budget
	caps     Caps
	// purgeAfter is how long a deprecated memory is kept before deletion. Zero
	// disables purging.
	purgeAfter time.Duration
	// now supplies the current time for time-aware prompts and for the purge
	// cutoff. Defaults to time.Now.
	now func() time.Time
	log *slog.Logger
}

// NewWorker creates a Worker with the default caps and a 30-day purge window.
func NewWorker(episodes EpisodeSource, mem memory.Port, llm LLM, budget Budget) *Worker {
	if budget.MaxTokens <= 0 {
		budget.MaxTokens = 100_000
	}
	return &Worker{
		episodes:   episodes,
		memory:     mem,
		llm:        llm,
		budget:     budget,
		caps:       DefaultCaps(),
		purgeAfter: 720 * time.Hour,
		now:        time.Now,
		log:        slog.Default(),
	}
}

// SetCaps overrides the per-kind live-memory caps (config DREAMING_MAX_*).
// A non-positive cap is ignored, so a partially-filled Caps cannot silently
// unbound a kind — config validation rejects those before they reach here, and
// this is the second line of the same defence.
func (w *Worker) SetCaps(c Caps) {
	if c.Facts > 0 {
		w.caps.Facts = c.Facts
	}
	if c.Insights > 0 {
		w.caps.Insights = c.Insights
	}
	if c.Summaries > 0 {
		w.caps.Summaries = c.Summaries
	}
}

// SetBudget overrides the token budget for subsequent runs (config
// DREAMING_MAX_TOKENS). A non-positive MaxTokens is ignored, matching the
// "there is no 'unbounded' setting" rule.
func (w *Worker) SetBudget(b Budget) {
	if b.MaxTokens > 0 {
		w.budget.MaxTokens = b.MaxTokens
	}
}

// SetPurgeAfter overrides the retention window for deprecated memories
// (config DREAMING_PURGE_AFTER). Zero or negative disables purging.
func (w *Worker) SetPurgeAfter(d time.Duration) { w.purgeAfter = d }

// SetClock overrides the worker's clock (tests; production uses time.Now).
func (w *Worker) SetClock(now func() time.Time) {
	if now != nil {
		w.now = now
	}
}

// SetLogger overrides the worker's logger.
func (w *Worker) SetLogger(l *slog.Logger) {
	if l != nil {
		w.log = l
	}
}

// Run performs one dreaming pass over every eligible session.
func (w *Worker) Run(ctx context.Context) (Result, error) {
	sessions, err := w.episodes.PendingSessions(ctx)
	if err != nil {
		return Result{}, err
	}
	return w.runOver(ctx, sessions)
}

// RunForUser performs one dreaming pass over a single account's eligible
// sessions. It is what the console's "consolidate now" triggers, so the scan is
// narrowed at the source rather than filtered afterwards — a pass must never
// read or spend on sessions the requester does not own.
//
// When the account has no unconsolidated sessions it COMPACTS instead: it runs
// consolidation over the existing store with no new material, so duplicates get
// merged and time-stale memories retired. Without this the button is a no-op
// exactly when a user reaches for it — their store is visibly untidy and there
// is no new conversation to hang the work on. "Consolidate" has to mean
// consolidate, not only "learn from new messages".
func (w *Worker) RunForUser(ctx context.Context, userID string) (Result, error) {
	sessions, err := w.episodes.PendingSessionsForUser(ctx, userID)
	if err != nil {
		return Result{}, err
	}
	res, err := w.runOver(ctx, sessions)
	if err != nil || res.EpisodesProcessed > 0 || res.BudgetExhausted {
		// Sessions were consolidated, so the whole store was already reviewed
		// against them — a second pass over it would pay again for the same work.
		return res, err
	}
	return w.compact(ctx, res, identity.UserScope(userID))
}

// compact reviews an existing store with no new material: merge what is
// duplicated, retire what time has made stale, then hold the caps. It is the
// only path that consolidates without an episode to learn from.
func (w *Worker) compact(ctx context.Context, res Result, scope identity.ScopeRef) (Result, error) {
	applied, tokens, err := w.consolidate(ctx, scope, nil, "")
	res.TokensUsed += tokens
	res.MemoriesWritten += applied.added
	res.MemoriesRevised += applied.revised
	res.MemoriesRetired += applied.retired
	res.Compacted = true
	if err != nil {
		return res, err
	}

	evicted, err := w.enforceCaps(ctx, scope)
	res.MemoriesRetired += evicted
	if err != nil {
		return res, err
	}
	res.MemoriesPurged = w.purge(ctx)
	return res, nil
}

// runOver is the pass itself, over an already-selected set of sessions.
func (w *Worker) runOver(ctx context.Context, sessions []PendingSession) (Result, error) {
	var res Result

	for _, sess := range sessions {
		if res.TokensUsed >= w.budget.MaxTokens {
			res.BudgetExhausted = true
			break
		}

		eps, err := w.episodes.Episodes(ctx, sess.ID, sess.Seq)
		if err != nil {
			return res, err
		}
		if len(eps) == 0 {
			// Race: the messages were consumed (or the session was deleted) between
			// the eligibility scan and now — nothing to do, and nothing to advance.
			continue
		}

		out, err := w.processSession(ctx, sess.Session, eps, w.budget.MaxTokens-res.TokensUsed)
		res.EpisodesProcessed += len(eps)
		res.MemoriesWritten += out.added
		res.MemoriesRevised += out.revised
		res.MemoriesRetired += out.retired
		res.TokensUsed += out.tokens
		if err != nil {
			return res, err
		}
		if !out.consolidated {
			// The batch was not folded in. Advancing the watermark here would mark
			// these episodes consumed and they would never be learned from — a
			// silent, permanent loss. Leave it: a later pass, with a fresh budget,
			// reads the same messages.
			res.BudgetExhausted = true
			w.log.Info("dreaming: batch deferred, watermark held",
				"session", sess.ID, "reason", out.skipReason)
			continue
		}

		// Advance the watermark to the newest message consumed. Episodes are
		// seq-ordered (and ids ascend with seq), so the last is the maximum.
		if err := w.episodes.MarkProcessed(ctx, sess.ID, eps[len(eps)-1].ID); err != nil {
			return res, err
		}
	}

	res.MemoriesPurged = w.purge(ctx)
	return res, nil
}

// sessionOutcome is what one session batch produced.
type sessionOutcome struct {
	added, revised, retired int
	tokens                  int
	// consolidated reports whether the consolidate stage actually ran. Only then
	// may the caller advance the watermark.
	consolidated bool
	skipReason   string
}

// processSession runs the pipeline for one batch of a session's episodes,
// staying within the remaining token budget:
//
//	EXTRACT facts → COMPRESS the batch to a summary → CONSOLIDATE both against
//	the scope's whole live store → enforce the caps.
//
// Each stage checks the remaining allowance before spending it, so the budget
// bounds work WITHIN a batch and not merely between batches.
func (w *Worker) processSession(ctx context.Context, sess session.Session, eps []session.StoredMessage, remaining int) (sessionOutcome, error) {
	scope := identity.UserScope(sess.UserID)
	var out sessionOutcome

	// A stage cannot know its cost before it runs; what it can know is whether
	// there is any allowance left to spend. Reserve enough for the stage that
	// must not be skipped — consolidation — before spending on the two that
	// feed it.
	if remaining <= 0 {
		out.skipReason = "no token allowance remaining"
		return out, nil
	}

	facts, tk, err := w.extract(ctx, eps)
	out.tokens += tk
	if err != nil {
		return out, err
	}

	var summary string
	if remaining-out.tokens > 0 {
		summary, tk, err = w.compress(ctx, eps)
		out.tokens += tk
		if err != nil {
			return out, err
		}
	}

	if remaining-out.tokens <= 0 {
		// Extract and compress exhausted the allowance. Their output is discarded
		// rather than stored half-folded, and the watermark stays put.
		out.skipReason = "allowance exhausted before consolidation"
		return out, nil
	}

	applied, tk, err := w.consolidate(ctx, scope, facts, summary)
	out.tokens += tk
	out.added, out.revised, out.retired = applied.added, applied.revised, applied.retired
	if err != nil {
		return out, err
	}
	out.consolidated = true

	evicted, err := w.enforceCaps(ctx, scope)
	if err != nil {
		return out, err
	}
	out.retired += evicted
	return out, nil
}

// extract uses the LLM to pull durable facts/preferences from episodes. It uses
// structured output (L3) so reasoning prose can never be parsed as a fact.
func (w *Worker) extract(ctx context.Context, eps []session.StoredMessage) ([]string, int, error) {
	var res extractResult
	tokens, err := w.llm.CompleteJSON(ctx, extractPrompt(episodeText(eps), w.today()), extractSchema, &res)
	if err != nil {
		return nil, tokens, err
	}
	return cleanLines(res.Facts), tokens, nil
}

// compress uses the LLM to condense a batch of episodes into one summary. The
// summary is not stored here — it is new material for consolidation, which
// decides whether it becomes a memory or merges into an existing one.
func (w *Worker) compress(ctx context.Context, eps []session.StoredMessage) (string, int, error) {
	var res summaryResult
	tokens, err := w.llm.CompleteJSON(ctx, summaryPrompt(episodeText(eps)), summarySchema, &res)
	if err != nil {
		return "", tokens, err
	}
	return strings.TrimSpace(res.Summary), tokens, nil
}

// applied counts what a consolidation actually changed.
type applied struct{ added, revised, retired int }

// consolidate folds the batch's new material into the scope's store in ONE LLM
// call: it hands over every live memory (handle-labelled, with the caps and
// current counts) plus the new facts and summary, and applies the edits it
// returns.
func (w *Worker) consolidate(ctx context.Context, scope identity.ScopeRef, facts []string, summary string) (applied, int, error) {
	var done applied

	all, err := w.memory.ListByScope(ctx, scope)
	if err != nil {
		return done, 0, err
	}
	live := liveOf(all)
	existing, byHandle := handles(live)

	// Nothing new and nothing to reorganize: skip the call rather than pay for a
	// model to confirm there is no work.
	if len(facts) == 0 && strings.TrimSpace(summary) == "" && len(existing) == 0 {
		return done, 0, nil
	}

	var res consolidateResult
	tokens, err := w.llm.CompleteJSON(ctx,
		consolidatePrompt(facts, summary, existing, w.caps, w.today()), consolidateSchema, &res)
	if err != nil {
		return done, tokens, err
	}

	// Order matters: update → add → remove. A merge is expressed as "update the
	// survivor, remove the absorbed"; applying the removal first would, on a
	// partial failure, leave the source gone and the target un-merged — the one
	// ordering that loses information.
	for _, op := range res.Update {
		m, ok := byHandle[strings.TrimSpace(op.ID)]
		if !ok {
			w.log.Warn("dreaming: consolidation referenced an unknown handle", "op", "update", "handle", op.ID)
			continue
		}
		content := strings.TrimSpace(op.Content)
		if content == "" {
			// An empty rewrite is not a deletion request; remove says that.
			w.log.Warn("dreaming: consolidation returned an empty rewrite", "handle", op.ID)
			continue
		}
		if err := w.memory.Update(ctx, m.ID, content); err != nil {
			return done, tokens, err
		}
		done.revised++
	}

	for _, op := range res.Add {
		content := strings.TrimSpace(op.Content)
		if content == "" {
			continue
		}
		kind, ok := parseKind(op.Kind)
		if !ok {
			w.log.Warn("dreaming: consolidation returned an unknown kind", "kind", op.Kind)
			continue
		}
		if _, err := w.memory.Store(ctx, memory.Memory{Scope: scope, Kind: kind, Content: content}); err != nil {
			return done, tokens, err
		}
		done.added++
	}

	for _, op := range res.Remove {
		m, ok := byHandle[strings.TrimSpace(op.ID)]
		if !ok {
			w.log.Warn("dreaming: consolidation referenced an unknown handle", "op", "remove", "handle", op.ID)
			continue
		}
		if err := w.memory.Deprecate(ctx, m.ID); err != nil {
			return done, tokens, err
		}
		done.retired++
	}

	// A pass that retires most of a store is either a genuine cleanup or a model
	// having a bad day. Both are worth seeing; the removals are deprecations, so
	// they are recoverable until the purge window closes.
	if len(existing) > 0 && done.retired*2 > len(existing) {
		w.log.Warn("dreaming: consolidation retired most of a scope's memories",
			"scope", scope.Scope, "retired", done.retired, "live_before", len(existing))
	}
	return done, tokens, nil
}

// enforceCaps brings every over-cap pool back under its ceiling by deprecating
// its oldest live memories, and returns how many it evicted. This runs after
// consolidation has had its chance to merge instead; the cap holds regardless
// of what consolidation returned.
func (w *Worker) enforceCaps(ctx context.Context, scope identity.ScopeRef) (int, error) {
	all, err := w.memory.ListByScope(ctx, scope)
	if err != nil {
		return 0, err
	}
	live := liveOf(all)

	evicted := 0
	for _, g := range []capGroup{groupFacts, groupInsights, groupSummaries} {
		for _, m := range overCap(live, g, w.caps.limit(g)) {
			if err := w.memory.Deprecate(ctx, m.ID); err != nil {
				return evicted, err
			}
			evicted++
			w.log.Info("dreaming: evicted over-cap memory",
				"pool", string(g), "kind", string(m.Kind), "id", m.ID, "created", m.CreatedAt)
		}
	}
	return evicted, nil
}

// purge deletes deprecated memories past the retention window. Failures are
// logged, not returned: a pass that consolidated correctly should not be
// reported as failed because housekeeping could not run.
func (w *Worker) purge(ctx context.Context) int {
	if w.purgeAfter <= 0 {
		return 0
	}
	n, err := w.memory.PurgeDeprecated(ctx, purgeCutoff(w.now(), w.purgeAfter))
	if err != nil {
		w.log.Warn("dreaming: purge of deprecated memories failed", "err", err)
		return 0
	}
	if n > 0 {
		w.log.Info("dreaming: purged deprecated memories", "count", n)
	}
	return n
}

// handles labels memories M1…Mn and returns both the labelled list (for the
// prompt) and the reverse map (for resolving what the model returns). The map
// is the whole point: an unknown handle resolves to nothing and is skipped,
// where the previous substring matching would silently edit whichever memory
// happened to contain the returned text.
//
// The list is sorted before labelling. ListByScope makes no ordering promise
// (the in-memory port iterates a map), and unstable handles would make the
// prompt differ run to run for an unchanged store — defeating prompt caching
// and making any failure unreproducible.
func handles(live []memory.Memory) ([]handled, map[string]memory.Memory) {
	ordered := make([]memory.Memory, len(live))
	copy(ordered, live)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
	})

	out := make([]handled, 0, len(ordered))
	byHandle := make(map[string]memory.Memory, len(ordered))
	for i, m := range ordered {
		h := "M" + strconv.Itoa(i+1)
		out = append(out, handled{handle: h, mem: m})
		byHandle[h] = m
	}
	return out, byHandle
}

// parseKind maps a model-supplied kind string onto a known Kind. The schema
// constrains it to an enum, but a schema is a request, not a guarantee.
func parseKind(s string) (memory.Kind, bool) {
	switch memory.Kind(strings.ToLower(strings.TrimSpace(s))) {
	case memory.KindFact:
		return memory.KindFact, true
	case memory.KindPreference:
		return memory.KindPreference, true
	case memory.KindInsight:
		return memory.KindInsight, true
	case memory.KindSummary:
		return memory.KindSummary, true
	}
	return "", false
}

// cleanLines trims and drops blank entries from a structured string list.
func cleanLines(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// episodeText renders a batch of episodes into the transcript both the extract
// and compress prompts consume. Each message carries its persisted timestamp so
// the model can anchor relative time ("next Saturday", "recently") to an
// absolute date when extracting durable facts (随时间保鲜). Text and thinking
// carry the prose; tool_use/tool_result name the action and its outcome.
func episodeText(eps []session.StoredMessage) string {
	var b strings.Builder
	for _, m := range eps {
		ts := m.CreatedAt.Format("[2006-01-02 15:04] ")
		for _, blk := range m.Content {
			switch blk.Type {
			case provider.BlockText:
				b.WriteString(ts + string(m.Role) + ": " + blk.Text + "\n")
			case provider.BlockThinking:
				b.WriteString(ts + string(m.Role) + " (thinking): " + blk.Thinking + "\n")
			case provider.BlockToolUse:
				b.WriteString(ts + "tool_use: " + blk.ToolName + "\n")
			case provider.BlockToolResult:
				b.WriteString(ts + "tool_result: " + blk.ToolContent + "\n")
			}
		}
	}
	return b.String()
}

// today returns the current date (YYYY-MM-DD) for time-aware prompts.
func (w *Worker) today() string {
	return w.now().Format("2006-01-02")
}
