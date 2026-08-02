package dreaming

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/session"
)

// Caps, budget and watermark safety (memory-consolidation). These are the
// invariants the store's growth depends on, so they are asserted against the
// machinery directly rather than through a model's cooperation.

// seedAt stores a memory with an explicit creation time. Eviction is
// oldest-first, and consecutive Store calls can land on the same instant on a
// coarse clock, so tests that care about order must set it.
func seedAt(t *testing.T, mem *memory.MemPort, user string, kind memory.Kind, content string, at time.Time) memory.Memory {
	t.Helper()
	m := seed(t, mem, user, kind, content)
	mem.Backdate(m.ID, at)
	return m
}

func liveKind(t *testing.T, mem memory.Port, user string, kind memory.Kind) []memory.Memory {
	t.Helper()
	all, err := mem.ListByScope(context.Background(), identity.UserScope(user))
	if err != nil {
		t.Fatalf("ListByScope: %v", err)
	}
	var out []memory.Memory
	for _, m := range all {
		if !m.Deprecated && m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// The machine half of cap enforcement: whatever consolidation returned, a pool
// over its ceiling loses its oldest members until it fits.
func TestEnforceCapsEvictsOldestFirst(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedAt(t, mem, "u1", memory.KindInsight,
			"insight "+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour))
	}

	w := NewWorker(&fakeEpisodeSource{}, mem, &fakeLLM{}, Budget{MaxTokens: 100})
	w.SetCaps(Caps{Insights: 2})

	evicted, err := w.enforceCaps(ctx, identity.UserScope("u1"))
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 3 {
		t.Errorf("evicted = %d want 3 (5 live, cap 2)", evicted)
	}
	live := liveKind(t, mem, "u1", memory.KindInsight)
	if len(live) != 2 {
		t.Fatalf("live insights = %d want 2", len(live))
	}
	// The two survivors are the newest.
	kept := map[string]bool{live[0].Content: true, live[1].Content: true}
	if !kept["insight d"] || !kept["insight e"] {
		t.Errorf("kept %v, want the two newest (insight d, insight e)", kept)
	}
}

// Per-kind caps exist so a freely-generating kind cannot consume the allowance
// of a rarely-generating one — the failure that put insights at 83% of a live
// store whose facts were the part with value.
func TestCapsDoNotCrowdOtherKinds(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		seedAt(t, mem, "u1", memory.KindInsight, "insight "+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour))
	}
	for i := 0; i < 3; i++ {
		seedAt(t, mem, "u1", memory.KindFact, "fact "+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour))
	}

	w := NewWorker(&fakeEpisodeSource{}, mem, &fakeLLM{}, Budget{MaxTokens: 100})
	w.SetCaps(Caps{Facts: 80, Insights: 2, Summaries: 40})

	if _, err := w.enforceCaps(ctx, identity.UserScope("u1")); err != nil {
		t.Fatal(err)
	}
	if n := len(liveKind(t, mem, "u1", memory.KindInsight)); n != 2 {
		t.Errorf("live insights = %d want 2 (its own cap)", n)
	}
	if n := len(liveKind(t, mem, "u1", memory.KindFact)); n != 3 {
		t.Errorf("live facts = %d want 3 (untouched by the insight cap)", n)
	}
}

// Deprecated memories are already invisible to recall. Counting them would make
// the cap tighten as a side effect of ordinary supersession, evicting live
// memories to make room for rows nothing can read.
func TestDeprecatedDoNotCountTowardCap(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		m := seedAt(t, mem, "u1", memory.KindInsight, "old "+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour))
		if err := mem.Deprecate(ctx, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	seedAt(t, mem, "u1", memory.KindInsight, "live one", base.Add(10*time.Hour))
	seedAt(t, mem, "u1", memory.KindInsight, "live two", base.Add(11*time.Hour))

	w := NewWorker(&fakeEpisodeSource{}, mem, &fakeLLM{}, Budget{MaxTokens: 100})
	w.SetCaps(Caps{Insights: 3})

	evicted, err := w.enforceCaps(ctx, identity.UserScope("u1"))
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 0 {
		t.Errorf("evicted = %d want 0: 2 live insights fit a cap of 3 regardless of 5 deprecated ones", evicted)
	}
}

// fact and preference draw on one pool: both are "things true about the user",
// and splitting them would force an arbitrary line between "prefers X" and
// "is X".
func TestFactAndPreferenceShareOnePool(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	seedAt(t, mem, "u1", memory.KindFact, "fact a", base)
	seedAt(t, mem, "u1", memory.KindFact, "fact b", base.Add(time.Hour))
	seedAt(t, mem, "u1", memory.KindPreference, "pref a", base.Add(2*time.Hour))
	seedAt(t, mem, "u1", memory.KindPreference, "pref b", base.Add(3*time.Hour))

	w := NewWorker(&fakeEpisodeSource{}, mem, &fakeLLM{}, Budget{MaxTokens: 100})
	w.SetCaps(Caps{Facts: 3})

	evicted, err := w.enforceCaps(ctx, identity.UserScope("u1"))
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 1 {
		t.Errorf("evicted = %d want 1 (4 across the shared pool, cap 3)", evicted)
	}
	// The oldest of the pool goes, whichever kind it happens to be.
	if n := len(liveKind(t, mem, "u1", memory.KindFact)); n != 1 {
		t.Errorf("live facts = %d want 1 (the oldest fact was evicted)", n)
	}
	if n := len(liveKind(t, mem, "u1", memory.KindPreference)); n != 2 {
		t.Errorf("live preferences = %d want 2", n)
	}
}

// A partially-filled Caps must not silently unbound a kind: an unbounded store
// is the failure caps exist to prevent, so zero means "unset", never "infinite".
func TestSetCapsIgnoresNonPositive(t *testing.T) {
	w := NewWorker(&fakeEpisodeSource{}, memory.NewMemPort(), &fakeLLM{}, Budget{MaxTokens: 100})
	w.SetCaps(Caps{Insights: 7}) // facts and summaries left at zero
	if w.caps.Insights != 7 {
		t.Errorf("insights cap = %d want 7", w.caps.Insights)
	}
	if w.caps.Facts != DefaultCaps().Facts || w.caps.Summaries != DefaultCaps().Summaries {
		t.Errorf("caps = %+v, want the untouched fields left at their defaults", w.caps)
	}
}

// Caps run after consolidation, so a model that ignores the ceiling still
// cannot grow the store past it.
func TestCapsHoldWhenConsolidationIgnoresThem(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"a"}},
		summaryResult{Summary: "s"},
		consolidateResult{Add: []addOp{
			{Kind: "insight", Content: "insight 1"},
			{Kind: "insight", Content: "insight 2"},
			{Kind: "insight", Content: "insight 3"},
			{Kind: "insight", Content: "insight 4"},
		}},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 1000})
	w.SetCaps(Caps{Insights: 2})

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(liveKind(t, mem, "u1", memory.KindInsight)); n != 2 {
		t.Errorf("live insights = %d want 2 despite the model adding 4", n)
	}
	// The eviction is reported, so a pass that fought the model is visible.
	if res.MemoriesRetired != 2 {
		t.Errorf("retired = %d want 2 (the over-cap evictions)", res.MemoriesRetired)
	}
}

// The budget must bound work WITHIN a batch, not merely between batches. The
// parameters that carried it used to be declared and never read, so the guard
// looked present and did nothing.
func TestBudgetDefersBatchAndHoldsWatermark(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1"), pending("s2", "u2")},
		episodes: map[string][]session.StoredMessage{
			"s1": {textMsg("a")},
			"s2": {textMsg("b")},
		},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"a fact"}},
		summaryResult{Summary: "s"},
		consolidateResult{},
		extractResult{Facts: []string{"another"}},
	}, tokens: 100}
	// 400 tokens buys s1's three calls (300) and only s2's extract (100).
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 400})

	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.BudgetExhausted {
		t.Error("expected the pass to report the budget exhausted")
	}
	if llm.calls != 4 {
		t.Errorf("llm calls = %d want 4 (s1 fully, s2 stopped after extract)", llm.calls)
	}
	// The critical assertion: s2's episodes were read and partly paid for, but
	// never consolidated. Advancing its watermark would consume them without
	// learning from them — a silent, permanent loss.
	if len(src.processed) != 1 || src.processed[0] != "s1" {
		t.Errorf("processed = %v, want only s1; s2's watermark must be held", src.processed)
	}
}

// A batch deferred for budget is retried on the next pass, with the same
// episodes still on offer.
func TestDeferredBatchIsRetried(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("a")}},
	}
	// One token of allowance: extract spends it, consolidation never runs.
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"a fact"}},
	}, tokens: 100}
	w := NewWorker(src, memory.NewMemPort(), llm, Budget{MaxTokens: 50})

	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(src.processed) != 0 {
		t.Fatalf("processed = %v, want none: nothing was consolidated", src.processed)
	}

	// A fresh pass with a real budget completes the same batch.
	llm.calls = 0
	llm.jsonResults = []any{
		extractResult{Facts: []string{"a fact"}},
		summaryResult{Summary: "s"},
		consolidateResult{Add: []addOp{{Kind: "fact", Content: "a fact"}}},
	}
	w.budget = Budget{MaxTokens: 1000}
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(src.processed) != 1 || src.processed[0] != "s1" {
		t.Errorf("processed = %v, want s1 consolidated on the retry", src.processed)
	}
}

// A batch handed no allowance at all does no work and reports why. Run's own
// loop breaks before it can produce this case, so it is asserted against
// processSession directly — the guard exists for any future caller, and an
// untested guard is a guess.
func TestZeroRemainingBudgetSkipsEverything(t *testing.T) {
	llm := &fakeLLM{tokens: 10}
	w := NewWorker(&fakeEpisodeSource{}, memory.NewMemPort(), llm, Budget{MaxTokens: 100})

	out, err := w.processSession(context.Background(),
		session.Session{ID: "s1", UserID: "u1"}, []session.StoredMessage{textMsg("a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 0 {
		t.Errorf("llm calls = %d want 0", llm.calls)
	}
	if out.consolidated {
		t.Error("nothing was consolidated, so the watermark must not advance")
	}
	if out.skipReason == "" {
		t.Error("a skipped batch should say why, or the deferral is invisible in the log")
	}
}

func TestPurgeRunsOncePerPass(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	stale := seed(t, mem, "u1", memory.KindFact, "retired long ago")
	keep := seed(t, mem, "u1", memory.KindFact, "still live")
	if err := mem.Deprecate(ctx, stale.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)

	w := NewWorker(&fakeEpisodeSource{}, mem, &fakeLLM{}, Budget{MaxTokens: 100})
	w.SetPurgeAfter(time.Millisecond)

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesPurged != 1 {
		t.Errorf("purged = %d want 1", res.MemoriesPurged)
	}
	if _, err := mem.GetByID(ctx, stale.ID); err == nil {
		t.Error("the retired memory should have been purged")
	}
	if _, err := mem.GetByID(ctx, keep.ID); err != nil {
		t.Errorf("a live memory was purged: %v", err)
	}
}

func TestPurgeDisabledWhenWindowIsZero(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	stale := seed(t, mem, "u1", memory.KindFact, "retired")
	if err := mem.Deprecate(ctx, stale.ID); err != nil {
		t.Fatal(err)
	}

	w := NewWorker(&fakeEpisodeSource{}, mem, &fakeLLM{}, Budget{MaxTokens: 100})
	w.SetPurgeAfter(0)

	res, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoriesPurged != 0 {
		t.Errorf("purged = %d want 0 with purging disabled", res.MemoriesPurged)
	}
	if _, err := mem.GetByID(ctx, stale.ID); err != nil {
		t.Errorf("nothing should have been purged: %v", err)
	}
}

// Handles must be stable for an unchanged store: ListByScope makes no ordering
// promise (the in-memory port iterates a map), and unstable handles would make
// the prompt differ run to run, defeating prompt caching and making any failure
// unreproducible.
func TestHandlesAreStableAcrossCalls(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	live := []memory.Memory{
		{ID: "c", Content: "third", CreatedAt: base.Add(2 * time.Hour)},
		{ID: "a", Content: "first", CreatedAt: base},
		{ID: "b", Content: "second", CreatedAt: base.Add(time.Hour)},
	}
	first, byHandle := handles(live)
	if first[0].mem.ID != "a" || first[1].mem.ID != "b" || first[2].mem.ID != "c" {
		t.Errorf("handles are not ordered oldest-first: %+v", first)
	}
	if byHandle["M1"].ID != "a" {
		t.Errorf("M1 = %q want a", byHandle["M1"].ID)
	}

	// Same input in a different order yields the same labelling.
	shuffled := []memory.Memory{live[2], live[0], live[1]}
	second, _ := handles(shuffled)
	for i := range first {
		if first[i].handle != second[i].handle || first[i].mem.ID != second[i].mem.ID {
			t.Fatalf("handle assignment is order-dependent: %v vs %v", first, second)
		}
	}
}
