// Package dreaming implements the dreaming capability (design D6): a scheduled
// offline worker that is the ONLY writer to long-term memory. It reads
// persisted run episodes for sessions with unconsolidated messages and
// consolidates them into user/team-scoped long-term memories via an extract →
// compress → reorganize → reflect pipeline, bounded by an LLM budget.
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
// Each call returns text and the tokens consumed (for budget accounting).
type LLM interface {
	Complete(ctx context.Context, prompt string) (string, int, error)
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
}

// NewWorker creates a Worker.
func NewWorker(episodes EpisodeSource, mem memory.Port, llm LLM, budget Budget) *Worker {
	if budget.MaxTokens <= 0 {
		budget.MaxTokens = 100_000
	}
	return &Worker{episodes: episodes, memory: mem, llm: llm, budget: budget}
}

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
func (w *Worker) processSession(ctx context.Context, sess session.Session, eps []session.StoredMessage, remainingBudget int) (int, int, error) {
	// 1. EXTRACT: episodes → facts/preferences.
	facts, tokens, err := w.extract(ctx, eps, remainingBudget)
	if err != nil {
		return 0, tokens, err
	}

	scope := identity.UserScope(sess.UserID)
	written := 0

	// 2. REORGANIZE: store new facts, deprecating contradicted memories.
	for _, f := range facts {
		if err := w.reorganize(ctx, scope, f); err != nil {
			return written, tokens, err
		}
		written++
	}
	return written, tokens, nil
}

// extract uses the LLM to pull durable facts/preferences from episodes.
func (w *Worker) extract(ctx context.Context, eps []session.StoredMessage, budget int) ([]string, int, error) {
	// Build the extraction prompt from the conversation's message blocks. Text
	// and thinking carry the prose; tool_use/tool_result name the action and its
	// outcome, so the extractor sees what the agent did as well as what it said.
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
	out, tokens, err := w.llm.Complete(ctx, extractPrompt(b.String()))
	if err != nil {
		return nil, tokens, err
	}
	return splitLines(out), tokens, nil
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
