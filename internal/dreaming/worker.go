// Package dreaming implements the dreaming capability (design D6): a scheduled
// offline worker that is the ONLY writer to long-term memory. It reads
// persisted run episodes for sessions with unconsolidated messages and
// consolidates them into user/team-scoped long-term memories via an extract →
// compress → reorganize → reflect pipeline, bounded by an LLM budget. The four
// stages write facts, per-batch summaries, and cross-memory insights
// (KindFact/KindSummary/KindInsight).
//
// Dreaming is INCREMENTAL (capability-gap K1, watermark model): each session
// carries a high-water mark (the messages.id consolidated up to), and the worker
// learns from the messages beyond it. A conversation therefore stays open and
// resumable while it is being learned from — learning no longer requires the
// session to end.
package dreaming

import (
	"context"
	"strings"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// LLM is the model the worker calls for extraction/summarization/reflection.
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
	TokensUsed        int
	BudgetExhausted   bool
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
	// enableReflect turns on the compress + reflect stages (KindSummary/
	// KindInsight). Off runs the cheaper extract → reorganize pipeline only.
	enableReflect bool
}

// NewWorker creates a Worker.
func NewWorker(episodes EpisodeSource, mem memory.Port, llm LLM, budget Budget) *Worker {
	if budget.MaxTokens <= 0 {
		budget.MaxTokens = 100_000
	}
	return &Worker{episodes: episodes, memory: mem, llm: llm, budget: budget, enableReflect: true}
}

// SetReflect toggles the compress + reflect stages (config DREAMING_REFLECT).
func (w *Worker) SetReflect(on bool) { w.enableReflect = on }

// Run performs one dreaming pass over eligible sessions.
func (w *Worker) Run(ctx context.Context) (Result, error) {
	var res Result

	sessions, err := w.episodes.PendingSessions(ctx)
	if err != nil {
		return res, err
	}

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

		written, tokens, err := w.processSession(ctx, sess.Session, eps, w.budget.MaxTokens-res.TokensUsed)
		if err != nil {
			return res, err
		}
		res.EpisodesProcessed += len(eps)
		res.MemoriesWritten += written
		res.TokensUsed += tokens

		// Advance the watermark to the newest message consumed. Episodes are
		// seq-ordered (and ids ascend with seq), so the last is the maximum.
		if err := w.episodes.MarkProcessed(ctx, sess.ID, eps[len(eps)-1].ID); err != nil {
			return res, err
		}
	}
	return res, nil
}

// processSession runs the pipeline for one batch of a session's episodes,
// staying within the remaining token budget. Returns memories written and
// tokens used.
//
// Stages (design D6): EXTRACT facts → COMPRESS the batch to a summary →
// REORGANIZE facts in (deprecating contradictions) → REFLECT over the summary
// plus existing memories to derive cross-memory insights and dedupe. The two
// LLM stages (extract/compress) run first so reflection can read the summary.
func (w *Worker) processSession(ctx context.Context, sess session.Session, eps []session.StoredMessage, remainingBudget int) (int, int, error) {
	scope := identity.UserScope(sess.UserID)
	tokens := 0
	written := 0

	// 1. EXTRACT: episodes → facts/preferences.
	facts, tk, err := w.extract(ctx, eps, remainingBudget)
	tokens += tk
	if err != nil {
		return 0, tokens, err
	}

	var summary string
	if w.enableReflect {
		// 2. COMPRESS: episodes → one summary memory of this batch.
		var tk2 int
		summary, tk2, err = w.compress(ctx, eps, remainingBudget-tokens)
		tokens += tk2
		if err != nil {
			return written, tokens, err
		}
		if summary != "" {
			if err := w.store(ctx, scope, memory.KindSummary, summary); err != nil {
				return written, tokens, err
			}
			written++
		}
	}

	// 3. REORGANIZE: store new facts, deprecating contradicted memories.
	for _, f := range facts {
		if err := w.reorganize(ctx, scope, f); err != nil {
			return written, tokens, err
		}
		written++
	}

	if w.enableReflect {
		// 4. REFLECT: summary + existing memories → insights + dedupe/deprecations.
		n, tk3, err := w.reflect(ctx, scope, summary, remainingBudget-tokens)
		tokens += tk3
		written += n
		if err != nil {
			return written, tokens, err
		}
	}
	return written, tokens, nil
}

// store persists one memory of the given kind in the scope.
func (w *Worker) store(ctx context.Context, scope identity.ScopeRef, kind memory.Kind, content string) error {
	_, err := w.memory.Store(ctx, memory.Memory{Scope: scope, Kind: kind, Content: content})
	return err
}

// compress uses the LLM to condense a batch of episodes into one running
// summary (the COMPRESS stage → memory.KindSummary).
func (w *Worker) compress(ctx context.Context, eps []session.StoredMessage, budget int) (string, int, error) {
	var res summaryResult
	tokens, err := w.llm.CompleteJSON(ctx, summaryPrompt(episodeText(eps)), summarySchema, &res)
	if err != nil {
		return "", tokens, err
	}
	return strings.TrimSpace(res.Summary), tokens, nil
}

// reflect derives cross-memory insights from the new batch summary plus the
// scope's existing memories, and deprecates memories the reflection flags as
// duplicated/superseded (the REFLECT stage → memory.KindInsight). Returns the
// number of insight memories written and the tokens used.
func (w *Worker) reflect(ctx context.Context, scope identity.ScopeRef, summary string, budget int) (int, int, error) {
	existing, err := w.memory.ListByScope(ctx, scope)
	if err != nil {
		return 0, 0, err
	}
	// Only reflect over live memories; skip when there's nothing but the summary
	// we just wrote and no new information to generalize from.
	var lines []string
	for _, m := range existing {
		if !m.Deprecated {
			lines = append(lines, m.Content)
		}
	}
	var res reflectResult
	tokens, err := w.llm.CompleteJSON(ctx, reflectPrompt(summary, lines), reflectSchema, &res)
	if err != nil {
		return 0, tokens, err
	}

	written := 0
	for _, content := range cleanLines(res.Insights) {
		if err := w.store(ctx, scope, memory.KindInsight, content); err != nil {
			return written, tokens, err
		}
		written++
	}
	for _, content := range cleanLines(res.Deprecate) {
		w.deprecateMatching(ctx, existing, content)
	}
	return written, tokens, nil
}

// deprecateMatching deprecates the first live memory whose content matches the
// reflected line (case-insensitive, after trimming). Reflection returns the
// memory's exact text, so an exact match is the norm; a substring match is a
// fallback for minor LLM paraphrase.
func (w *Worker) deprecateMatching(ctx context.Context, existing []memory.Memory, content string) {
	norm := strings.ToLower(strings.TrimSpace(content))
	for _, m := range existing {
		if m.Deprecated {
			continue
		}
		c := strings.ToLower(strings.TrimSpace(m.Content))
		if c == norm || strings.Contains(c, norm) {
			_ = w.memory.Deprecate(ctx, m.ID) // best-effort; a miss only skips dedupe
			return
		}
	}
}

// extract uses the LLM to pull durable facts/preferences from episodes. It uses
// structured output (L3) so reasoning prose can never be parsed as a fact.
func (w *Worker) extract(ctx context.Context, eps []session.StoredMessage, budget int) ([]string, int, error) {
	var res extractResult
	tokens, err := w.llm.CompleteJSON(ctx, extractPrompt(episodeText(eps)), extractSchema, &res)
	if err != nil {
		return nil, tokens, err
	}
	return cleanLines(res.Facts), tokens, nil
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
// and compress prompts consume. Text and thinking carry the prose;
// tool_use/tool_result name the action and its outcome, so the model sees what
// the agent did as well as what it said.
func episodeText(eps []session.StoredMessage) string {
	var b strings.Builder
	for _, m := range eps {
		for _, blk := range m.Content {
			switch blk.Type {
			case provider.BlockText:
				b.WriteString(string(m.Role) + ": " + blk.Text + "\n")
			case provider.BlockThinking:
				b.WriteString(string(m.Role) + " (thinking): " + blk.Thinking + "\n")
			case provider.BlockToolUse:
				b.WriteString("tool_use: " + blk.ToolName + "\n")
			case provider.BlockToolResult:
				b.WriteString("tool_result: " + blk.ToolContent + "\n")
			}
		}
	}
	return b.String()
}

// reorganize stores a fact, deprecating any existing memory it contradicts.
func (w *Worker) reorganize(ctx context.Context, scope identity.ScopeRef, fact string) error {
	existing, err := w.memory.ListByScope(ctx, scope)
	if err != nil {
		return err
	}
	for _, m := range existing {
		if !m.Deprecated && contradicts(m.Content, fact) {
			if err := w.memory.Deprecate(ctx, m.ID); err != nil {
				return err
			}
		}
	}
	_, err = w.memory.Store(ctx, memory.Memory{
		Scope:   scope,
		Kind:    memory.KindFact,
		Content: fact,
	})
	return err
}
