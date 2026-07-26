package dreaming

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// fakeLLM returns canned output and records token usage. The worker now uses
// structured output (CompleteJSON); jsonResults queues one value per call and
// is marshalled into the out pointer. outputs/output drive the legacy Complete
// (unused by the worker now, kept for the interface).
type fakeLLM struct {
	jsonResults []any
	output      string
	outputs     []string
	tokens      int
	calls       int
	err         error
}

func (f *fakeLLM) Complete(_ context.Context, _ string) (string, int, error) {
	out := f.output
	if f.calls < len(f.outputs) {
		out = f.outputs[f.calls]
	}
	f.calls++
	return out, f.tokens, f.err
}

func (f *fakeLLM) CompleteJSON(_ context.Context, _ string, _ *provider.JSONResponseSpec, out any) (int, error) {
	if f.calls < len(f.jsonResults) {
		res := f.jsonResults[f.calls]
		if err := remarshal(res, out); err != nil {
			return f.tokens, err
		}
	}
	f.calls++
	return f.tokens, f.err
}

// remarshal copies a canned value into the out pointer via JSON round-trip.
func remarshal(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
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
	// extract → 2 facts; compress → a summary; reflect → nothing.
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user likes go", "prefers dark mode"}},
		summaryResult{Summary: "talked about go"},
		reflectResult{},
	}, tokens: 50}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.TokensUsed != 150 {
		t.Errorf("tokens = %d want 150 (3 calls x 50)", res.TokensUsed)
	}
	if res.EpisodesProcessed != 1 {
		t.Errorf("episodes = %d", res.EpisodesProcessed)
	}
	if llm.calls != 3 {
		t.Errorf("llm calls = %d want 3 (extract+compress+reflect)", llm.calls)
	}

	// Facts + summary stored under the user's scope.
	stored, _ := mem.ListByScope(context.Background(), identity.UserScope("user1"))
	var facts, summaries int
	for _, m := range stored {
		switch m.Kind {
		case memory.KindFact:
			facts++
		case memory.KindSummary:
			summaries++
		}
	}
	if facts != 2 {
		t.Errorf("facts = %d want 2", facts)
	}
	if summaries != 1 {
		t.Errorf("summaries = %d want 1", summaries)
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
	// Budget allows exactly one full session: 3 calls (extract+compress+reflect)
	// at 100 tokens each = 300, then the budget check stops session s2.
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 300})

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.BudgetExhausted {
		t.Error("expected budget exhausted")
	}
	if llm.calls != 3 {
		t.Errorf("llm calls = %d want 3 (one full session, then budget)", llm.calls)
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
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user no longer uses vim"}},
		summaryResult{Summary: "editor preferences discussed"},
		reflectResult{},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 100})

	if _, err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}

	stored, _ := mem.ListByScope(ctx, identity.UserScope("u1"))
	// Only the pre-existing "user uses vim" fact is deprecated (by reorganize's
	// contradiction heuristic); the summary and the new fact stay live.
	var deprecatedFacts int
	for _, m := range stored {
		if m.Deprecated && m.Kind == memory.KindFact {
			deprecatedFacts++
		}
	}
	if deprecatedFacts != 1 {
		t.Errorf("expected 1 deprecated fact, got %d: %+v", deprecatedFacts, stored)
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

// TestWorkerReflectWritesInsightAndDedupes: the reflect stage derives a
// KindInsight from the batch summary + existing memories, and deprecates an
// existing memory it flags as duplicated — this is what dedupes facts the
// incremental extractor re-derives across batches.
func TestWorkerReflectWritesInsightAndDedupes(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	// Pre-existing duplicate fact the reflection should deprecate.
	mem.Store(ctx, memory.Memory{Scope: identity.UserScope("u1"), Kind: memory.KindFact, Content: "user likes go"})

	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("user likes go and uses it daily")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user likes go"}},
		summaryResult{Summary: "user codes in go daily"},
		reflectResult{Insights: []string{"user is an active gopher"}, Deprecate: []string{"user likes go"}},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	stored, _ := mem.ListByScope(ctx, identity.UserScope("u1"))
	var insights, deprecated int
	for _, m := range stored {
		if m.Kind == memory.KindInsight {
			insights++
		}
		if m.Deprecated {
			deprecated++
		}
	}
	if insights != 1 {
		t.Errorf("insights = %d want 1, stored = %+v", insights, stored)
	}
	if deprecated != 1 {
		t.Errorf("deprecated = %d want 1 (the duplicate fact), stored = %+v", deprecated, stored)
	}
	// 1 summary + 1 fact + 1 insight written.
	if res.MemoriesWritten != 3 {
		t.Errorf("memories written = %d want 3", res.MemoriesWritten)
	}
}

func TestCleanLines(t *testing.T) {
	got := cleanLines([]string{"  a ", "", "  ", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("cleanLines = %v", got)
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
