// Package dreaming implements the dreaming capability (design D6): a scheduled
// offline worker that is the ONLY writer to long-term memory. It reads
// persisted run episodes for ended sessions and consolidates them into
// user/team-scoped long-term memories via an extract → compress → reorganize →
// reflect pipeline, bounded by an LLM budget.
package dreaming

import (
	"context"
	"strings"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
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

// EpisodeSource provides episodes (persisted run events) for ended sessions.
type EpisodeSource interface {
	// EndedSessions returns sessions eligible for dreaming (ended, unprocessed).
	EndedSessions(ctx context.Context) ([]session.Session, error)
	// Episodes returns the run events for a session, ordered.
	Episodes(ctx context.Context, sessionID string) ([]session.Event, error)
	// MarkProcessed records that a session's episodes were dreamed over.
	MarkProcessed(ctx context.Context, sessionID string) error
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

	sessions, err := w.episodes.EndedSessions(ctx)
	if err != nil {
		return res, err
	}

	for _, sess := range sessions {
		if res.TokensUsed >= w.budget.MaxTokens {
			res.BudgetExhausted = true
			break
		}

		eps, err := w.episodes.Episodes(ctx, sess.ID)
		if err != nil {
			return res, err
		}
		if len(eps) == 0 {
			_ = w.episodes.MarkProcessed(ctx, sess.ID)
			continue
		}

		written, tokens, err := w.processSession(ctx, sess, eps, w.budget.MaxTokens-res.TokensUsed)
		if err != nil {
			return res, err
		}
		res.EpisodesProcessed += len(eps)
		res.MemoriesWritten += written
		res.TokensUsed += tokens

		if err := w.episodes.MarkProcessed(ctx, sess.ID); err != nil {
			return res, err
		}
	}
	return res, nil
}

// processSession runs the pipeline for one session's episodes, staying within
// the remaining token budget. Returns memories written and tokens used.
func (w *Worker) processSession(ctx context.Context, sess session.Session, eps []session.Event, remainingBudget int) (int, int, error) {
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
func (w *Worker) extract(ctx context.Context, eps []session.Event, budget int) ([]string, int, error) {
	// Build the extraction prompt from episode payloads.
	var b strings.Builder
	for _, e := range eps {
		b.WriteString(string(e.Payload))
		b.WriteString("\n")
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
