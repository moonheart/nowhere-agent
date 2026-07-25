package dreaming

import (
	"context"
	"errors"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// fakeLLM returns canned output and records token usage.
type fakeLLM struct {
	output string
	tokens int
	calls  int
	err    error
}

func (f *fakeLLM) Complete(_ context.Context, _ string) (string, int, error) {
	f.calls++
	return f.output, f.tokens, f.err
}

// fakeEpisodeSource serves canned pending sessions + episodes (watermark model).
type fakeEpisodeSource struct {
	sessions  []PendingSession
	episodes  map[string][]session.StoredMessage
	processed []string
	lastSeq   map[string]int64
}

func (f *fakeEpisodeSource) PendingSessions(context.Context) ([]PendingSession, error) {
	return f.sessions, nil
}
func (f *fakeEpisodeSource) Episodes(_ context.Context, id string, _ int64) ([]session.StoredMessage, error) {
	return f.episodes[id], nil
}
func (f *fakeEpisodeSource) MarkProcessed(_ context.Context, id string, seq int64) error {
	f.processed = append(f.processed, id)
	if f.lastSeq == nil {
		f.lastSeq = map[string]int64{}
	}
	f.lastSeq[id] = seq
	return nil
}

// textMsg builds a stored user text message for tests.
func textMsg(text string) session.StoredMessage {
	return session.StoredMessage{
		Role:    provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: text}},
	}
}

func pending(id, userID string) PendingSession {
	return PendingSession{Session: session.Session{ID: id, UserID: userID, Status: session.SessionActive}, Seq: 0}
}

func TestWorkerExtractsAndStoresFacts(t *testing.T) {
	sess := pending("s1", "user1")
	src := &fakeEpisodeSource{
		sessions: []PendingSession{sess},
		episodes: map[string][]session.StoredMessage{
			"s1": {textMsg("user likes go")},
		},
	}
	mem := memory.NewMemPort()
	llm := &fakeLLM{output: "- user likes go\n- prefers dark mode", tokens: 50}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesWritten != 2 {
		t.Errorf("memories written = %d want 2", res.MemoriesWritten)
	}
	if res.TokensUsed != 50 {
		t.Errorf("tokens = %d want 50", res.TokensUsed)
	}
	if res.EpisodesProcessed != 1 {
		t.Errorf("episodes = %d", res.EpisodesProcessed)
	}

	// Facts stored under the user's scope.
	stored, _ := mem.ListByScope(context.Background(), identity.UserScope("user1"))
	if len(stored) != 2 {
		t.Errorf("stored memories = %d want 2", len(stored))
	}
	// Session marked processed.
	if len(src.processed) != 1 || src.processed[0] != "s1" {
		t.Errorf("processed = %v", src.processed)
	}
}

func TestWorkerBudgetStopsProcessing(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1"), pending("s2", "u2")},
		episodes: map[string][]session.StoredMessage{
			"s1": {textMsg("a")},
			"s2": {textMsg("b")},
		},
	}
	llm := &fakeLLM{output: "fact", tokens: 100}
	// Budget only allows one session's worth.
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 100})

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.BudgetExhausted {
		t.Error("expected budget exhausted")
	}
	if llm.calls != 1 {
		t.Errorf("llm calls = %d want 1 (budget)", llm.calls)
	}
}

func TestWorkerReorganizeDeprecatesContradicted(t *testing.T) {
	mem := memory.NewMemPort()
	ctx := context.Background()
	// Existing memory that will be contradicted.
	mem.Store(ctx, memory.Memory{Scope: identity.UserScope("u1"), Kind: memory.KindFact, Content: "user uses vim"})

	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{output: "user no longer uses vim", tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 100})

	if _, err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}

	stored, _ := mem.ListByScope(ctx, identity.UserScope("u1"))
	var deprecated int
	for _, m := range stored {
		if m.Deprecated {
			deprecated++
		}
	}
	if deprecated != 1 {
		t.Errorf("expected 1 deprecated memory, got %d: %+v", deprecated, stored)
	}
}

// TestWorkerEmptyEpisodesSkips: a session that races to empty (messages consumed
// between scan and read) is skipped WITHOUT advancing a watermark.
func TestWorkerEmptyEpisodesSkips(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {}},
	}
	w := NewWorker(src, memory.NewMemPort(), &fakeLLM{}, Budget{MaxTokens: 100})
	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesWritten != 0 || res.EpisodesProcessed != 0 {
		t.Errorf("expected a no-op pass, got %+v", res)
	}
	if len(src.processed) != 0 {
		t.Errorf("empty episodes must not advance the watermark, processed = %v", src.processed)
	}
}

func TestWorkerLLMErrorPropagates(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{err: errors.New("llm down"), tokens: 5}
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 100})
	if _, err := w.Run(context.Background()); err == nil {
		t.Error("expected llm error to propagate")
	}
}

// TestWorkerAdvancesWatermarkToNewestEpisode: after a pass the watermark lands
// on the newest consumed message id (ids ascend with seq).
func TestWorkerAdvancesWatermarkToNewestEpisode(t *testing.T) {
	ep1 := textMsg("first")
	ep1.ID = 10
	ep2 := textMsg("second")
	ep2.ID = 20
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {ep1, ep2}},
	}
	w := NewWorker(src, memory.NewMemPort(), &fakeLLM{output: "fact", tokens: 5}, Budget{MaxTokens: 100})
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.lastSeq["s1"] != 20 {
		t.Errorf("watermark = %d want 20 (newest episode id)", src.lastSeq["s1"])
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines("- a\n\n- b\n  \n")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitLines = %v", got)
	}
}

func TestContradicts(t *testing.T) {
	if contradicts("a", "a") {
		t.Error("identical should not contradict")
	}
	if !contradicts("uses vim", "no longer uses vim") {
		t.Error("negation should contradict")
	}
}
