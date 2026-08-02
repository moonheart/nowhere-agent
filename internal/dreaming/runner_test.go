package dreaming

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/session"
)

// blockingSource holds a pass open until release is closed, so a test can
// observe the single-flight lock while it is genuinely held rather than by
// guessing at timing.
type blockingSource struct {
	*fakeEpisodeSource
	entered chan struct{}
	release chan struct{}
	once    bool
}

func newBlockingSource(inner *fakeEpisodeSource) *blockingSource {
	return &blockingSource{
		fakeEpisodeSource: inner,
		entered:           make(chan struct{}, 1),
		release:           make(chan struct{}),
	}
}

func (b *blockingSource) block() {
	if b.once {
		return
	}
	b.once = true
	b.entered <- struct{}{}
	<-b.release
}

func (b *blockingSource) PendingSessions(ctx context.Context) ([]PendingSession, error) {
	b.block()
	return b.fakeEpisodeSource.PendingSessions(ctx)
}

func (b *blockingSource) PendingSessionsForUser(ctx context.Context, userID string) ([]PendingSession, error) {
	b.block()
	return b.fakeEpisodeSource.PendingSessionsForUser(ctx, userID)
}

func newTestRunner(t *testing.T, src EpisodeSource, mem memory.Port, llm LLM) *Runner {
	t.Helper()
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 10000})
	r := NewRunner(w, context.Background())
	t.Cleanup(r.Wait)
	return r
}

func TestRunnerTriggerRecordsResult(t *testing.T) {
	mem := memory.NewMemPort()
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("hello")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"user says hello"}},
		summaryResult{Summary: "a greeting"},
		consolidateResult{Add: []addOp{{Kind: "fact", Content: "user greets in English"}}},
	}, tokens: 30}
	r := newTestRunner(t, src, mem, llm)

	if err := r.TriggerForUser("u1"); err != nil {
		t.Fatalf("TriggerForUser: %v", err)
	}
	r.Wait()

	st := r.Status("u1")
	if st.Running {
		t.Error("no pass should be in flight after Wait")
	}
	if st.Last == nil {
		t.Fatal("the completed pass should be recorded")
	}
	if st.Last.Err != "" {
		t.Errorf("err = %q, want none", st.Last.Err)
	}
	if st.Last.Result.MemoriesWritten != 1 || st.Last.Result.TokensUsed != 90 {
		t.Errorf("result = %+v, want 1 memory and 90 tokens", st.Last.Result)
	}
	if st.Last.FinishedAt.Before(st.Last.StartedAt) {
		t.Error("FinishedAt precedes StartedAt")
	}
}

// Two passes must never overlap. The watermark makes a pass idempotent against
// itself, not against a second pass racing it: both would read the same
// dreamed_seq before either advanced it, consolidate the same messages, and
// leave the store with a duplicate set of memories from one episode.
func TestRunnerSecondTriggerIsBusy(t *testing.T) {
	src := newBlockingSource(&fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	})
	r := newTestRunner(t, src, memory.NewMemPort(), &fakeLLM{tokens: 1})

	if err := r.TriggerForUser("u1"); err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	<-src.entered // the pass is now genuinely in flight

	if err := r.TriggerForUser("u1"); !errors.Is(err, ErrBusy) {
		t.Errorf("second trigger err = %v, want ErrBusy", err)
	}
	st := r.Status("u1")
	if !st.Running || !st.Mine {
		t.Errorf("status = %+v, want running and mine", st)
	}

	close(src.release)
	r.Wait()

	// Once the lock is free the next trigger is accepted again.
	if err := r.TriggerForUser("u1"); err != nil {
		t.Errorf("trigger after completion: %v", err)
	}
	r.Wait()
}

// A user must not be able to start a pass while ANOTHER user's is running: the
// two would race on the same watermarks if their sessions overlapped in the
// scheduled pass, and serializing is cheaper than reasoning about when they do.
func TestRunnerBusyAcrossUsers(t *testing.T) {
	src := newBlockingSource(&fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	})
	r := newTestRunner(t, src, memory.NewMemPort(), &fakeLLM{tokens: 1})

	if err := r.TriggerForUser("u1"); err != nil {
		t.Fatal(err)
	}
	<-src.entered

	if err := r.TriggerForUser("u2"); !errors.Is(err, ErrBusy) {
		t.Errorf("other user's trigger err = %v, want ErrBusy", err)
	}
	// u2 sees a pass running, but not one of theirs.
	if st := r.Status("u2"); !st.Running || st.Mine {
		t.Errorf("u2 status = %+v, want running but not mine", st)
	}

	close(src.release)
	r.Wait()
}

// A missed scheduled tick is not a failure: the next one picks up whatever this
// one would have, and returning an error would light up the scheduler's error
// path for ordinary contention.
func TestRunnerScheduledSkipsWhenBusy(t *testing.T) {
	src := newBlockingSource(&fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	})
	llm := &fakeLLM{tokens: 1}
	r := newTestRunner(t, src, memory.NewMemPort(), llm)

	if err := r.TriggerForUser("u1"); err != nil {
		t.Fatal(err)
	}
	<-src.entered

	before := llm.calls
	if err := r.RunScheduled(context.Background()); err != nil {
		t.Errorf("scheduled pass returned %v, want nil (a skip is not an error)", err)
	}
	if llm.calls != before {
		t.Errorf("the scheduled pass ran anyway: calls %d → %d", before, llm.calls)
	}

	close(src.release)
	r.Wait()
}

func TestRunnerManualBlockedByScheduled(t *testing.T) {
	src := newBlockingSource(&fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	})
	r := newTestRunner(t, src, memory.NewMemPort(), &fakeLLM{tokens: 1})

	done := make(chan error, 1)
	go func() { done <- r.RunScheduled(context.Background()) }()
	<-src.entered

	if err := r.TriggerForUser("u1"); !errors.Is(err, ErrBusy) {
		t.Errorf("manual trigger err = %v, want ErrBusy while the scheduled pass runs", err)
	}

	close(src.release)
	if err := <-done; err != nil {
		t.Fatalf("scheduled pass: %v", err)
	}
}

func TestRunnerRecordsFailure(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{err: errors.New("provider unreachable"), tokens: 5}
	r := newTestRunner(t, src, memory.NewMemPort(), llm)

	if err := r.TriggerForUser("u1"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	st := r.Status("u1")
	if st.Last == nil || st.Last.Err == "" {
		t.Fatalf("the failure should be recorded, got %+v", st.Last)
	}
	// A failed pass must release the lock, or one provider outage kills manual
	// consolidation until the process restarts.
	if st.Running {
		t.Error("a failed pass left the single-flight lock held")
	}
	if err := r.TriggerForUser("u1"); err != nil {
		t.Errorf("trigger after a failed pass: %v", err)
	}
	r.Wait()
}

func TestRunnerRejectsEmptyUser(t *testing.T) {
	r := newTestRunner(t, &fakeEpisodeSource{}, memory.NewMemPort(), &fakeLLM{})
	if err := r.TriggerForUser(""); err == nil {
		t.Error("an empty user id must not start a pass")
	}
	if st := r.Status(""); st.Running {
		t.Error("no pass should be running")
	}
}

// A caller who has never triggered a pass has no history — and specifically is
// not shown someone else's.
func TestRunnerStatusIsPerCaller(t *testing.T) {
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	r := newTestRunner(t, src, memory.NewMemPort(), &fakeLLM{tokens: 1})

	if err := r.TriggerForUser("u1"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	if r.Status("u1").Last == nil {
		t.Error("u1 should see their own pass")
	}
	if r.Status("u2").Last != nil {
		t.Error("u2 must not see u1's pass")
	}
}

// A store full of duplicates with no unconsolidated sessions is exactly when a
// user reaches for "Consolidate now". Before compaction existed the pass looped
// over zero sessions and reported "nothing to consolidate" — truthful about the
// code and wrong about the button.
func TestRunForUserCompactsWhenNoPendingSessions(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewMemPort()
	seed(t, mem, "u1", memory.KindFact, "The user has a cat named Doudou (豆豆).")
	seed(t, mem, "u1", memory.KindFact, "用户养了一只叫豆豆的猫。")

	// No pending sessions at all.
	src := &fakeEpisodeSource{}
	llm := &fakeLLM{jsonResults: []any{
		consolidateResult{
			Update: []updateOp{{ID: "M1", Content: "The user has a cat named Doudou (豆豆)."}},
			Remove: []removeOp{{ID: "M2", Reason: "same fact in Chinese, merged into M1"}},
		},
	}, tokens: 20}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 10000})

	res, err := w.RunForUser(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Compacted {
		t.Error("the pass should report that it compacted the store")
	}
	if res.EpisodesProcessed != 0 {
		t.Errorf("episodes = %d want 0 (there were none)", res.EpisodesProcessed)
	}
	// One consolidate call — extract and compress have no transcript to read.
	if llm.calls != 1 {
		t.Errorf("llm calls = %d want 1 (consolidate only)", llm.calls)
	}
	if live := liveContents(t, mem, "u1"); len(live) != 1 {
		t.Errorf("live = %v, want the two duplicates merged into one", live)
	}
	if res.MemoriesRevised != 1 || res.MemoriesRetired != 1 {
		t.Errorf("revised=%d retired=%d, want 1 and 1", res.MemoriesRevised, res.MemoriesRetired)
	}
}

// Compaction is the fallback, not an extra cost on every pass: when sessions
// were consolidated, the whole store was already reviewed against them.
func TestRunForUserSkipsCompactionAfterConsolidating(t *testing.T) {
	mem := memory.NewMemPort()
	seed(t, mem, "u1", memory.KindFact, "an existing memory")
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("s1", "u1")},
		episodes: map[string][]session.StoredMessage{"s1": {textMsg("x")}},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"a fact"}},
		summaryResult{Summary: "s"},
		consolidateResult{},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 10000})

	res, err := w.RunForUser(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Compacted {
		t.Error("a pass that consolidated sessions should not also compact")
	}
	if llm.calls != 3 {
		t.Errorf("llm calls = %d want 3 (extract+compress+consolidate, no extra compaction)", llm.calls)
	}
}

// An empty store has nothing to compact, and paying a model to confirm that on
// every button press would be a standing cost for no result.
func TestRunForUserCompactionSkipsEmptyStore(t *testing.T) {
	llm := &fakeLLM{tokens: 10}
	w := NewWorker(&fakeEpisodeSource{}, memory.NewMemPort(), llm, Budget{MaxTokens: 10000})

	res, err := w.RunForUser(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 0 {
		t.Errorf("llm calls = %d want 0 for an empty store", llm.calls)
	}
	if res.TokensUsed != 0 {
		t.Errorf("tokens = %d want 0", res.TokensUsed)
	}
}

// The compaction prompt has to name its own job. Handed "fold the new material
// in" with no new material, a model answers with empty arrays.
func TestCompactionPromptAsksForCleanup(t *testing.T) {
	mem := memory.NewMemPort()
	seed(t, mem, "u1", memory.KindFact, "The user has a cat named Doudou (豆豆).")
	llm := &fakeLLM{jsonResults: []any{consolidateResult{}}, tokens: 5}
	w := NewWorker(&fakeEpisodeSource{}, mem, llm, Budget{MaxTokens: 10000})

	if _, err := w.RunForUser(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if len(llm.prompts) != 1 {
		t.Fatalf("prompts = %d want 1", len(llm.prompts))
	}
	p := llm.prompts[0]
	for _, want := range []string{
		"NO new material",
		"DIFFERENT LANGUAGES", // the case that prompted this: same fact, two languages
		"M1: The user has a cat named Doudou",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("compaction prompt missing %q", want)
		}
	}
	if strings.Contains(p, "Fold the new material into the store") {
		t.Error("compaction prompt still asks the model to fold in material that does not exist")
	}
}

// The fidelity rule exists because a live compaction merged three memories that
// all said the cat was named 豆豆 into one claiming it was named 欢欢, and
// invented "the user corrected the earlier belief" to explain the change. A
// consolidation prompt that does not forbid introducing facts will get them.
func TestConsolidatePromptForbidsInventingFacts(t *testing.T) {
	mem := memory.NewMemPort()
	seed(t, mem, "u1", memory.KindFact, "The user has a cat named Doudou (豆豆).")
	llm := &fakeLLM{jsonResults: []any{consolidateResult{}}, tokens: 5}
	w := NewWorker(&fakeEpisodeSource{}, mem, llm, Budget{MaxTokens: 10000})

	if _, err := w.RunForUser(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	p := llm.prompts[0]
	for _, want := range []string{
		"FIDELITY",
		"NEVER introduce a name",
		"NEVER invent a change of state",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("consolidate prompt missing the fidelity rule %q", want)
		}
	}
}

func TestRunnerSetTimeoutIgnoresNonPositive(t *testing.T) {
	r := newTestRunner(t, &fakeEpisodeSource{}, memory.NewMemPort(), &fakeLLM{})
	before := r.timeout
	r.SetTimeout(0)
	r.SetTimeout(-time.Second)
	if r.timeout != before {
		t.Errorf("timeout = %v, want the default kept (%v)", r.timeout, before)
	}
	r.SetTimeout(time.Minute)
	if r.timeout != time.Minute {
		t.Errorf("timeout = %v, want 1m", r.timeout)
	}
}

// The narrowing is the authorization boundary: a user pressing "consolidate my
// memories" must not cause another account's conversations to be read, or their
// tokens to be spent on it.
func TestRunForUserOnlyTouchesOwnSessions(t *testing.T) {
	mem := memory.NewMemPort()
	src := &fakeEpisodeSource{
		sessions: []PendingSession{pending("mine", "u1"), pending("theirs", "u2")},
		episodes: map[string][]session.StoredMessage{
			"mine":   {textMsg("my conversation")},
			"theirs": {textMsg("their conversation")},
		},
	}
	llm := &fakeLLM{jsonResults: []any{
		extractResult{Facts: []string{"a fact"}},
		summaryResult{Summary: "s"},
		consolidateResult{Add: []addOp{{Kind: "fact", Content: "learned from u1"}}},
	}, tokens: 10}
	w := NewWorker(src, mem, llm, Budget{MaxTokens: 10000})

	res, err := w.RunForUser(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.EpisodesProcessed != 1 {
		t.Errorf("episodes = %d want 1 (only u1's session)", res.EpisodesProcessed)
	}
	if len(src.processed) != 1 || src.processed[0] != "mine" {
		t.Errorf("processed = %v, want only u1's session", src.processed)
	}
	if got := len(liveContents(t, mem, "u2")); got != 0 {
		t.Errorf("u2 gained %d memories from u1's trigger", got)
	}
}
