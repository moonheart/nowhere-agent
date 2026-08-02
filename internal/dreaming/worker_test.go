package dreaming

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// fakeLLM returns canned output and records token usage. The worker uses
// structured output (CompleteJSON); jsonResults queues one value per call and
// is marshalled into the out pointer. outputs/output drive the legacy Complete
// (unused by the worker, kept for the interface). prompts records each
// CompleteJSON prompt so tests can assert what the model was shown.
type fakeLLM struct {
	jsonResults []any
	output      string
	outputs     []string
	tokens      int
	calls       int
	err         error
	prompts     []string
}

func (f *fakeLLM) Complete(_ context.Context, _ string) (string, int, error) {
	out := f.output
	if f.calls < len(f.outputs) {
		out = f.outputs[f.calls]
	}
	f.calls++
	return out, f.tokens, f.err
}

func (f *fakeLLM) CompleteJSON(_ context.Context, prompt string, _ *provider.JSONResponseSpec, out any) (int, error) {
	f.prompts = append(f.prompts, prompt)
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

// PendingSessionsForUser mirrors the production narrowing: only the requester's
// own sessions come back.
func (f *fakeEpisodeSource) PendingSessionsForUser(_ context.Context, userID string) ([]PendingSession, error) {
	var out []PendingSession
	for _, s := range f.sessions {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
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

// seed stores a memory and returns it, failing the test on error.
func seed(t *testing.T, mem memory.Port, user string, kind memory.Kind, content string) memory.Memory {
	t.Helper()
	m, err := mem.Store(context.Background(), memory.Memory{
		Scope: identity.UserScope(user), Kind: kind, Content: content,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return m
}

// liveContents returns the live memory contents in a scope, for assertions.
func liveContents(t *testing.T, mem memory.Port, user string) []string {
	t.Helper()
	all, err := mem.ListByScope(context.Background(), identity.UserScope(user))
	if err != nil {
		t.Fatalf("ListByScope: %v", err)
	}
	var out []string
	for _, m := range all {
		if !m.Deprecated {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestWorkerExtractsAndConsolidates(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "user1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("user likes go")}},
	}
	mem := memory.NewMemPort()
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user likes go", "prefers dark mode"}},
		summaryResult{Summary: "talked about go"},
		consolidateResult{Add: []addOp{
			{Kind: "fact", Content: "user likes go"},
			{Kind: "preference", Content: "prefers dark mode"},
			{Kind: "summary", Content: "talked about go"},
		}},
	}, tokens: 50}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Three calls, not 2+F: consolidation is one call whatever the batch yielded.
	if llm.calls != 3 {
		t.Errorf("llm calls = %d want 3 (extract+compress+consolidate)", llm.calls)
	}
	if res.TokensUsed != 150 {
		t.Errorf("tokens = %d want 150 (3 calls x 50)", res.TokensUsed)
	}
	if res.EpisodesProcessed != 1 {
		t.Errorf("episodes = %d want 1", res.EpisodesProcessed)
	}
	if res.MemoriesWritten != 3 {
		t.Errorf("written = %d want 3", res.MemoriesWritten)
	}
	if got := len(liveContents(t, mem, "user1")); got != 3 {
		t.Errorf("live memories = %d want 3", got)
	}
	if len(src.processed) != 1 || src.processed[0] != "s1" {
		t.Errorf("processed = %v", src.processed)
	}
}

// The cost defect this change exists to fix: the old pipeline made one
// full-store LLM call per extracted fact, so a chatty session cost a multiple
// of a quiet one. Consolidation is one call regardless.
func TestConsolidationIsOneCallRegardlessOfFactCount(t *testing.T) {
	many := make([]string, 20)
	for i := range many {
		many[i] = "fact number " + string(rune('a'+i))
	}
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: many},
		summaryResult{Summary: "s"},
		consolidateResult{},
	}, tokens: 1}
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 10000})

	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 3 {
		t.Errorf("llm calls = %d want 3 even with %d facts", llm.calls, len(many))
	}
}

func TestConsolidateUpdatesInPlace(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	existing := seed(t, mem, "u1", memory.KindFact, "user is planning a trip")

	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("the trip was great")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"the trip happened"}},
		summaryResult{Summary: "trip recap"},
		consolidateResult{Update: []updateOp{{ID: "M1", Content: "user took a trip in July 2026"}}},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesRevised != 1 {
		t.Errorf("revised = %d want 1", res.MemoriesRevised)
	}
	got, err := mem.GetByID(ctx, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "user took a trip in July 2026" {
		t.Errorf("content = %q, want the revised text", got.Content)
	}
	// Revision, not accumulation: the store still holds ONE memory, which is the
	// whole point of having an update op.
	if live := liveContents(t, mem, "u1"); len(live) != 1 {
		t.Errorf("live = %v, want exactly the one revised memory", live)
	}
}

func TestConsolidateRemoveDeprecates(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	stale := seed(t, mem, "u1", memory.KindFact, "user uses vim")

	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("switched editors")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user switched to helix"}},
		summaryResult{Summary: "editors"},
		consolidateResult{
			Add:    []addOp{{Kind: "fact", Content: "user uses helix as of 2026-07-31"}},
			Remove: []removeOp{{ID: "M1", Reason: "superseded by the helix fact"}},
		},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesRetired != 1 {
		t.Errorf("retired = %d want 1", res.MemoriesRetired)
	}
	// Retirement is a deprecation, not an erasure — recoverable until purge.
	got, err := mem.GetByID(ctx, stale.ID)
	if err != nil {
		t.Fatalf("the retired memory should still exist: %v", err)
	}
	if !got.Deprecated {
		t.Error("the retired memory should be deprecated")
	}
	if live := liveContents(t, mem, "u1"); len(live) != 1 || live[0] != "user uses helix as of 2026-07-31" {
		t.Errorf("live = %v, want only the new fact", live)
	}
}

// A merge is "update the survivor, remove the absorbed" — the case in-place
// revision was added for. One live memory must remain, carrying both dates.
func TestConsolidateMergeLeavesOneLive(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	first := seed(t, mem, "u1", memory.KindSummary, "user asked about the tool list (7/29)")
	time.Sleep(2 * time.Millisecond) // keep handle order deterministic by CreatedAt
	seed(t, mem, "u1", memory.KindSummary, "user asked about the tool list (7/31)")

	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("tools?")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{},
		summaryResult{Summary: "asked about tools again"},
		consolidateResult{
			Update: []updateOp{{ID: "M1", Content: "user has repeatedly asked for the tool list (7/29, 7/31)"}},
			Remove: []removeOp{{ID: "M2", Reason: "merged into M1"}},
		},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	if _, err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	live := liveContents(t, mem, "u1")
	if len(live) != 1 {
		t.Fatalf("live = %v, want exactly one merged memory", live)
	}
	if live[0] != "user has repeatedly asked for the tool list (7/29, 7/31)" {
		t.Errorf("merged content = %q", live[0])
	}
	got, err := mem.GetByID(ctx, first.ID)
	if err != nil || got.Deprecated {
		t.Errorf("the survivor should be the updated M1, got %+v (err %v)", got, err)
	}
}

// A handle the model invents must not resolve to anything. The pipeline this
// replaced matched returned text against memory content with a substring
// fallback and no length floor, so a short paraphrase could retire an arbitrary
// unrelated memory. Every other op in the same response must still apply.
func TestConsolidateUnknownHandleIgnored(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	keep := seed(t, mem, "u1", memory.KindFact, "user lives in Shanghai")

	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user has a cat"}},
		summaryResult{Summary: "s"},
		consolidateResult{
			Update: []updateOp{{ID: "M99", Content: "should not be applied"}},
			Add:    []addOp{{Kind: "fact", Content: "user has a cat named Doudou"}},
			Remove: []removeOp{{ID: "M42", Reason: "does not exist"}},
		},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesRevised != 0 || res.MemoriesRetired != 0 {
		t.Errorf("unknown handles should apply nothing, got revised=%d retired=%d",
			res.MemoriesRevised, res.MemoriesRetired)
	}
	// The valid op in the same response still applied.
	if res.MemoriesWritten != 1 {
		t.Errorf("written = %d want 1 (the valid add)", res.MemoriesWritten)
	}
	got, err := mem.GetByID(ctx, keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "user lives in Shanghai" || got.Deprecated {
		t.Errorf("the existing memory was touched by an unknown handle: %+v", got)
	}
}

// An empty rewrite is not a deletion request — `remove` says that. Applying it
// would blank a memory rather than retire it, leaving an empty row in recall.
func TestConsolidateEmptyRewriteIgnored(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	m := seed(t, mem, "u1", memory.KindFact, "user lives in Shanghai")

	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{},
		summaryResult{},
		consolidateResult{Update: []updateOp{{ID: "M1", Content: "   "}}},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	if _, err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.GetByID(ctx, m.ID)
	if got.Content != "user lives in Shanghai" {
		t.Errorf("content = %q, want it untouched by an empty rewrite", got.Content)
	}
}

func TestConsolidateUnknownKindIgnored(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	mem := memory.NewMemPort()
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"something worth folding in"}},
		summaryResult{Summary: "s"},
		consolidateResult{Add: []addOp{
			{Kind: "speculation", Content: "not a real kind"},
			{Kind: "fact", Content: "a real one"},
		}},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesWritten != 1 {
		t.Errorf("written = %d want 1 (the unknown kind is dropped)", res.MemoriesWritten)
	}
	if live := liveContents(t, mem, "u1"); len(live) != 1 || live[0] != "a real one" {
		t.Errorf("live = %v", live)
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
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"a fact"}},
		summaryResult{Summary: "s"},
		consolidateResult{},
	}, tokens: 5}
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 1000})
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.lastSeq["s1"] != 20 {
		t.Errorf("watermark = %d want 20 (newest episode id)", src.lastSeq["s1"])
	}
}

func TestCleanLines(t *testing.T) {
	got := cleanLines([]string{"  a ", "", "  ", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("cleanLines = %v", got)
	}
}

// fixedClock returns a clock pinned to a date, for time-injection assertions.
func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }
}

// TestWorkerInjectsTodayIntoPrompts: the extract + consolidate prompts carry the
// worker's clock date so the model can anchor time (随时间保鲜).
func TestWorkerInjectsTodayIntoPrompts(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("user likes go")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user likes go"}},
		summaryResult{Summary: "s"},
		consolidateResult{},
	}, tokens: 10}
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 1000})
	w.SetClock(fixedClock())

	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(llm.prompts) != 3 {
		t.Fatalf("prompts = %d want 3 (extract+compress+consolidate)", len(llm.prompts))
	}
	// extract (0) and consolidate (2) carry the date; the compress prompt (1)
	// does not — it is a pure condensation of an already-timestamped transcript.
	for _, i := range []int{0, 2} {
		if !strings.Contains(llm.prompts[i], "2026-07-26") {
			t.Errorf("prompt %d missing today's date", i)
		}
	}
}

// TestEpisodeTextRendersMessageTime: each transcript line carries the message's
// persisted timestamp so the model can anchor relative time.
func TestEpisodeTextRendersMessageTime(t *testing.T) {
	m := textMsg("hello")
	m.CreatedAt = time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)
	out := episodeText([]session.StoredMessage{m})
	if !strings.Contains(out, "[2026-07-20 08:30]") {
		t.Errorf("episode text missing message timestamp: %q", out)
	}
}

// The consolidation prompt must show the model what it is editing and what room
// it has: handles it can address, and each pool's live count against its cap.
func TestConsolidatePromptShowsHandlesAndCaps(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	mem := memory.NewMemPort()
	seed(t, mem, "u1", memory.KindFact, "user lives in Shanghai")
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user has a cat"}},
		summaryResult{Summary: "cats"},
		consolidateResult{},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})
	w.SetCaps(Caps{Facts: 5, Insights: 3, Summaries: 4})

	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := llm.prompts[2]
	for _, want := range []string{
		"M1: user lives in Shanghai", // the handle it addresses memories by
		"1 live of 5",                // the fact pool's count against its cap
		"0 live of 3",                // insights: empty groups are still shown
		"user has a cat",             // the new material
		"summary: cats",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("consolidate prompt missing %q", want)
		}
	}
	// The subject rule is what stops the store filling with commentary about
	// itself, so it must actually reach the model.
	if !strings.Contains(p, "describes the USER") {
		t.Error("consolidate prompt does not state that memories describe the user")
	}
}
