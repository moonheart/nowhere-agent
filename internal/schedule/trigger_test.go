package schedule

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// --- fakes -----------------------------------------------------------------

// stubProvider is a minimal adapter that emits one text block then stops, so a
// fired run reaches a terminal state without a real LLM.
type stubProvider struct{ gate <-chan struct{} }

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 16)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	go func() {
		defer close(ch)
		if p.gate != nil {
			select {
			case <-ctx.Done():
				return
			case <-p.gate:
			}
		}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "ok"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	}()
	return ch, nil
}

// captureProvider records the messages of the first Stream request it sees, so
// a test can assert what the fired run actually sent to the model.
type captureProvider struct {
	stubProvider
	seen chan []provider.Message
}

func (p *captureProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	select {
	case p.seen <- req.Messages:
	default:
	}
	return p.stubProvider.Stream(ctx, req)
}

type fakeScopes struct{ scopes []identity.ScopeRef }

func (f fakeScopes) AccessibleScopes(ctx context.Context, userID string) ([]identity.ScopeRef, error) {
	return f.scopes, nil
}

type fakeDefs struct {
	def agentdef.AgentDef
	err error
}

func (f fakeDefs) Resolve(name string, scopes []identity.ScopeRef) (agentdef.AgentDef, error) {
	if f.err != nil {
		return agentdef.AgentDef{}, f.err
	}
	return f.def, nil
}

// loopSpy records how many loops were built and hands back a loop that runs to
// completion against the stub provider.
type loopSpy struct {
	count  int32
	system atomic.Value
	gate   <-chan struct{} // when set, the run blocks until closed
}

func (s *loopSpy) build(ctx context.Context, task Task, system, model string) (*agent.Loop, error) {
	atomic.AddInt32(&s.count, 1)
	s.system.Store(system)
	return agent.New(&stubProvider{gate: s.gate}, toolruntime.NewRegistry(), agent.Config{System: system, Model: model, MaxTokens: 100}), nil
}

// --- helpers ---------------------------------------------------------------

// newRuntime wires a Runtime + RunRegistry over the dev DB, with a throwaway
// user. Returns the pieces and registers cleanup.
func newRuntime(t *testing.T, db *sql.DB) (*session.Runtime, *session.RunRegistry, string) {
	t.Helper()
	userID := pgNewUser(t, db)
	rt := session.NewRuntime(session.NewPGStore(db))
	rg := session.NewRunRegistry(rt, rt.Bus())
	return rt, rg, userID
}

// --- tests -----------------------------------------------------------------

// TestTriggerFiresDueTask is the end-to-end path: a due task is claimed and a
// run is submitted, producing a session tagged with the task.
func TestTriggerFiresDueTask(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)
	_ = rt

	store := NewPGStore(db)
	created, err := store.Create(context.Background(), validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	// Make it due now.
	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	tr.fireOnce(context.Background(), created)

	if atomic.LoadInt32(&spy.count) != 1 {
		t.Fatalf("expected 1 loop built, got %d", spy.count)
	}

	// A session was produced and tagged to the task.
	ids, err := store.ListSessions(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 produced session, got %v", ids)
	}
	var source string
	db.QueryRow(`SELECT source FROM sessions WHERE id = $1`, ids[0]).Scan(&source)
	if source != "scheduled" {
		t.Fatalf("session source = %q, want scheduled", source)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, ids[0]) })

	// The claim advanced next_run_at into the future.
	after, _ := store.Get(context.Background(), created.ID)
	if !after.NextRunAt.After(now) {
		t.Fatalf("claim did not advance next_run_at: %v", after.NextRunAt)
	}
}

// TestTriggerSkipsWhenClaimLost: a task already claimed (next_run_at advanced)
// is not fired again — the multi-instance guarantee from the trigger's side.
func TestTriggerSkipsWhenClaimLost(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	created, err := store.Create(context.Background(), validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	// Claim it once so it's no longer due.
	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)
	if _, err := store.Claim(context.Background(), created.ID, now); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	err = tr.fireOnce(context.Background(), created)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("lost claim should surface ErrNotFound, got %v", err)
	}
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("no loop should be built on a lost claim, got %d", spy.count)
	}
}

// TestTriggerAgentDefPromptSource: an agent-referencing task resolves its
// system prompt and model from the definition at fire time.
func TestTriggerAgentDefPromptSource(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	def := agentdef.AgentDef{Name: "reviewer", System: "You review code.", Model: "opus"}
	task := validTask(userID)
	task.Prompt = "review the latest changes"
	task.AgentDefName = "reviewer"

	store := NewPGStore(db)
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{def: def}, fakeScopes{}, spy.build, db, time.Hour)

	system, model, kickoff, err := tr.resolvePrompt(context.Background(), created)
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if system != "You review code." || model != "opus" || kickoff != "review the latest changes" {
		t.Fatalf("resolved = (%q,%q,%q)", system, model, kickoff)
	}
}

// TestTriggerRejectBusySession: under multitask=reject, a fire against a
// session with an active run is skipped and its fresh session cleaned up.
func TestTriggerRejectBusySession(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	// A target session with an active run (blocked on a gate so it stays active).
	sess, err := rt.CreateSession(context.Background(), userID, "busy target")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID) })
	gate := make(chan struct{})
	defer close(gate) // release the blocked run at test end
	msg := provider.TextMessage(provider.RoleUser, "hold")
	if _, err := rg.Submit(context.Background(), sess.ID, session.RunWork{
		Loop:        agent.New(&stubProvider{gate: gate}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100}),
		UserMessage: &msg,
	}); err != nil {
		t.Fatalf("submit blocking run: %v", err)
	}

	task := validTask(userID)
	task.TargetSessionID = sess.ID
	task.Multitask = MultitaskReject
	store := NewPGStore(db)
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)

	// Make the task due, then claim; the multitask gate blocks the submit.
	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)
	claimed, err := store.Claim(context.Background(), created.ID, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := tr.submit(context.Background(), claimed); err != nil {
		t.Fatalf("submit should skip cleanly, got %v", err)
	}
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("busy session under reject must not build a loop, got %d", spy.count)
	}
}

// TestTriggerSkipsPendingInteraction: a fire against a session with an
// undecided interaction is skipped (capability suspend-batch-snapshot) — a
// scheduled run must not bury a human's pending approval under newer turns.
func TestTriggerSkipsPendingInteraction(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)
	sessStore := session.NewPGStore(db)

	sess, err := rt.CreateSession(context.Background(), userID, "pending target")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID) })
	run, err := sessStore.CreateRun(context.Background(), sess.ID, 1)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := sessStore.CreateApproval(context.Background(), session.Interaction{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "danger", Kind: session.KindToolApproval,
	}); err != nil {
		t.Fatalf("create approval: %v", err)
	}
	// The suspended run settles done; only the pending interaction remains.
	if err := sessStore.UpdateRunStatus(context.Background(), run.ID, session.RunDone); err != nil {
		t.Fatalf("settle run: %v", err)
	}

	task := validTask(userID)
	task.TargetSessionID = sess.ID
	store := NewPGStore(db)
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)

	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)
	claimed, err := store.Claim(context.Background(), created.ID, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := tr.submit(context.Background(), claimed); err != nil {
		t.Fatalf("submit should skip cleanly, got %v", err)
	}
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("a session with a pending interaction must not build a loop, got %d", spy.count)
	}
	runs, err := sessStore.RunsForSession(context.Background(), sess.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("a skipped firing must not start a run; runs = %d, want 1", len(runs))
	}
}

// TestTriggerFireNowLeavesScheduleAlone: a manual run fires the task but does
// not claim it, so next_run_at/last_run_at stay exactly as the cadence left
// them — an out-of-band fire must not disturb the cron schedule.
func TestTriggerFireNowLeavesScheduleAlone(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	created, err := store.Create(context.Background(), validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })
	before, _ := store.Get(context.Background(), created.ID)

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	if err := tr.FireNow(context.Background(), created); err != nil {
		t.Fatalf("firenow: %v", err)
	}
	t.Cleanup(func() {
		ids, _ := store.ListSessions(context.Background(), created.ID)
		for _, id := range ids {
			db.Exec(`DELETE FROM sessions WHERE id = $1`, id)
		}
	})

	if atomic.LoadInt32(&spy.count) != 1 {
		t.Fatalf("manual fire should build one loop, got %d", spy.count)
	}
	// It produced a session (the run fired).
	if ids, _ := store.ListSessions(context.Background(), created.ID); len(ids) != 1 {
		t.Fatalf("expected 1 produced session, got %v", ids)
	}
	// But the schedule is untouched: same next_run_at, no last_run_at.
	after, _ := store.Get(context.Background(), created.ID)
	if !after.NextRunAt.Equal(before.NextRunAt) {
		t.Fatalf("manual fire moved next_run_at: %v -> %v", before.NextRunAt, after.NextRunAt)
	}
	if after.LastRunAt != nil {
		t.Fatalf("manual fire stamped last_run_at: %v", after.LastRunAt)
	}
}

// TestTriggerKickoffReachesModel is the regression for the empty-messages bug:
// a fired free-text task must send its kickoff as the opening user turn, not an
// empty history. The registry runs History verbatim (UserMessage is persisted,
// not merged in), so History must carry the kickoff.
func TestTriggerKickoffReachesModel(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	task := validTask(userID)
	task.Prompt = "summarize yesterday"
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)

	cap := &captureProvider{seen: make(chan []provider.Message, 1)}
	build := func(ctx context.Context, task Task, system, model string) (*agent.Loop, error) {
		return agent.New(cap, toolruntime.NewRegistry(), agent.Config{System: system, Model: model, MaxTokens: 100}), nil
	}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, build, db, time.Hour)
	tr.fireOnce(context.Background(), created)
	t.Cleanup(func() {
		ids, _ := store.ListSessions(context.Background(), created.ID)
		for _, id := range ids {
			db.Exec(`DELETE FROM sessions WHERE id = $1`, id)
		}
	})

	select {
	case msgs := <-cap.seen:
		if len(msgs) != 1 || msgs[0].Role != provider.RoleUser {
			t.Fatalf("first request should carry exactly the kickoff user turn, got %d msgs: %+v", len(msgs), msgs)
		}
		var text string
		for _, b := range msgs[0].Content {
			text += b.Text
		}
		if text != "summarize yesterday" {
			t.Fatalf("kickoff text = %q, want the task prompt", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Stream request reached the provider")
	}
}

// TestTriggerSweepFiresOnlyDue covers the scan path: the sweep fires due tasks
// and ignores ones that aren't due.
func TestTriggerSweepFiresOnlyDue(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	due, err := store.Create(context.Background(), validTask(userID))
	if err != nil {
		t.Fatalf("create due: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), due.ID) })
	notDue, err := store.Create(context.Background(), validTask(userID))
	if err != nil {
		t.Fatalf("create notDue: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), notDue.ID) })

	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), due.ID)
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(time.Hour), notDue.ID)

	spy := &loopSpy{}
	// Confine the sweep's due scan to this test's tasks. The trigger scans the
	// whole table, so leftover overdue tasks from other tests (or an interrupted
	// run's cleanup) would otherwise be fired here and inflate the count.
	scoped := &dueScopedStore{Store: store, ids: map[string]bool{due.ID: true, notDue.ID: true}}
	tr := NewTrigger(scoped, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	tr.sweep(context.Background())

	if atomic.LoadInt32(&spy.count) != 1 {
		t.Fatalf("sweep should fire exactly the one due task, built %d loops", spy.count)
	}

	// Clean up the produced session.
	ids, _ := store.ListSessions(context.Background(), due.ID)
	for _, id := range ids {
		db.Exec(`DELETE FROM sessions WHERE id = $1`, id)
	}
}

// dueScopedStore narrows ListDue to a fixed set of task ids, so a sweep test is
// insulated from overdue tasks other tests left in the shared dev database.
// Every other method delegates to the wrapped store.
type dueScopedStore struct {
	Store
	ids map[string]bool
}

func (s *dueScopedStore) ListDue(ctx context.Context, now time.Time) ([]Task, error) {
	due, err := s.Store.ListDue(ctx, now)
	if err != nil {
		return nil, err
	}
	out := due[:0]
	for _, t := range due {
		if s.ids[t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}
