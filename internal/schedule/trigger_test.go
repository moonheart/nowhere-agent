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
	"nowhere-agent/internal/quota"
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

// stuckTool blocks on a gate forever, ignoring ctx cancellation — the
// stand-in for a tool call stuck in an unbounded operation (a hung network
// call) that never observes the interrupt. Its hour-long timeout keeps the
// call alive well past interruptWaitTimeout. entered (when non-nil) is closed
// once Call is entered, so a test can wait until the run is genuinely parked
// inside the tool.
type stuckTool struct {
	gate    <-chan struct{}
	entered chan struct{}
}

func (t stuckTool) Name() string        { return "stuck" }
func (t stuckTool) Description() string { return "blocks until released" }
func (t stuckTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t stuckTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (t stuckTool) Timeout() time.Duration { return time.Hour }
func (t stuckTool) Call(_ context.Context, _ map[string]any) (toolruntime.Result, error) {
	if t.entered != nil {
		close(t.entered)
	}
	<-t.gate
	return toolruntime.Result{Content: "released"}, nil
}

// toolUseScriptProvider emits one tool_use turn (dispatching "stuck"), then a
// plain text turn once the tool's result is fed back — enough to drive a run
// that parks inside the stuck tool call.
type toolUseScriptProvider struct{}

func (p toolUseScriptProvider) Name() string { return "script" }

func (p toolUseScriptProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 16)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{
		Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "stuck", ToolInput: map[string]any{},
	}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "{}"}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopToolUse}
	close(ch)
	return ch, nil
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
	rg := session.NewRunRegistry(rt)
	return rt, rg, userID
}

// --- tests -----------------------------------------------------------------

// TestTriggerRejectsForeignTargetSession pins the IDOR gate (mirror of the
// inbound-webhook ownership check): a task may only fire into the OWNER's
// sessions — pointing it at another user's session must fail the fire without
// building a loop or touching the foreign session.
func TestTriggerRejectsForeignTargetSession(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	victimID := pgNewUser(t, db)
	victimSess, err := rt.CreateSession(context.Background(), victimID, "victim's private session")
	if err != nil {
		t.Fatalf("create victim session: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, victimSess.ID) })

	task := validTask(userID)
	task.TargetSessionID = victimSess.ID
	store := NewPGStore(db)
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	if err := tr.submit(context.Background(), created, false); err == nil {
		t.Fatal("fire into a foreign session must fail")
	}
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("loop built for a foreign session: %d", spy.count)
	}
	// The victim's session must be untouched (no new runs).
	runs, err := rt.RunsForSession(context.Background(), victimSess.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("victim session saw %d runs, want 0 (err %v)", len(runs), err)
	}
}

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
// TestTriggerInterruptCancelsActiveRun pins that multitask=interrupt REALLY
// stops the active run's worker before the new run starts — not just the
// runtime lock (which left worker A executing, burning tokens and
// interleaving its frames with run B's stream). The interrupted run must
// settle RunCancelled and the interrupting run must run to completion.
func TestTriggerInterruptCancelsActiveRun(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	// A target session with a run blocked on a gate so it stays active.
	sess, err := rt.CreateSession(context.Background(), userID, "interrupt target")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID) })
	gate := make(chan struct{})
	defer close(gate) // release the blocked run at test end
	msg := provider.TextMessage(provider.RoleUser, "hold")
	runA, err := rg.Submit(context.Background(), sess.ID, session.RunWork{
		Loop:        agent.New(&stubProvider{gate: gate}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100}),
		UserMessage: &msg,
	})
	if err != nil {
		t.Fatalf("submit blocking run: %v", err)
	}

	task := validTask(userID)
	task.TargetSessionID = sess.ID
	task.Multitask = MultitaskInterrupt
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
	if err := tr.submit(context.Background(), claimed, true); err != nil {
		t.Fatalf("submit under interrupt: %v", err)
	}

	// Both runs must settle: A cancelled (its worker was interrupted) and the
	// new run B done. Poll the durable statuses — B runs on its own goroutine.
	sessStore := session.NewPGStore(db)
	deadline := time.Now().Add(10 * time.Second)
	var aStatus, bStatus session.RunStatus
	for {
		runs, err := sessStore.RunsForSession(context.Background(), sess.ID)
		if err != nil {
			t.Fatalf("runs: %v", err)
		}
		if len(runs) == 2 {
			for _, r := range runs {
				if r.ID == runA.ID {
					aStatus = r.Status
				} else {
					bStatus = r.Status
				}
			}
			if aStatus == session.RunCancelled && bStatus == session.RunDone {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs never settled: a=%s b=%s runs=%d", aStatus, bStatus, len(runs))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTriggerInterruptTimeoutRequeuesFiring pins the interrupt-timeout path: a
// worker that ignores the cancel and stays active past interruptWaitTimeout
// makes the new fire's Submit fail with ErrRunActive. The claim already
// advanced, so a silent skip would defer this firing to the NEXT cron
// occurrence — the fix requeues it so the next scan retries.
func TestTriggerInterruptTimeoutRequeuesFiring(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	sess, err := rt.CreateSession(context.Background(), userID, "stuck target")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, sess.ID) })

	// Run A is parked inside a tool call that ignores ctx cancellation; it
	// cannot unwind until the gate closes (at test end).
	gate := make(chan struct{})
	entered := make(chan struct{})
	reg := toolruntime.NewRegistry()
	reg.Register(stuckTool{gate: gate, entered: entered})
	msg := provider.TextMessage(provider.RoleUser, "hold")
	if _, err := rg.Submit(context.Background(), sess.ID, session.RunWork{
		Loop:        agent.New(toolUseScriptProvider{}, reg, agent.Config{Model: "m", MaxTokens: 100}),
		UserMessage: &msg,
	}); err != nil {
		t.Fatalf("submit stuck run: %v", err)
	}
	select {
	case <-entered: // run A is now genuinely parked inside the stuck tool
	case <-time.After(5 * time.Second):
		t.Fatal("run never entered the stuck tool")
	}

	task := validTask(userID)
	task.TargetSessionID = sess.ID
	task.Multitask = MultitaskInterrupt
	store := NewPGStore(db)
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)
	claimed, err := store.Claim(context.Background(), created.ID, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	if err := tr.submit(context.Background(), claimed, true); err != nil {
		t.Fatalf("submit under interrupt timeout: %v", err)
	}

	// The stuck worker still holds the lock: no new run may have started.
	runs, err := rt.RunsForSession(context.Background(), sess.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("interrupt-timeout fire started a run; runs = %d, want 1 (err %v)", len(runs), err)
	}

	// The claimed slot was not burned: next_run_at is back to <= now, so the
	// next scan retries this firing instead of waiting for the next cron.
	after, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after skip: %v", err)
	}
	if after.NextRunAt.After(time.Now()) {
		t.Fatalf("interrupt-timeout task next_run_at = %v, want requeued to <= now", after.NextRunAt)
	}

	close(gate) // release the stuck run so the worker unwinds at test end
}

// TestTriggerRejectBusySession pins the reject branch: a busy target under
// multitask=reject skips the fire without building a loop.
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
	if err := tr.submit(context.Background(), claimed, true); err != nil {
		t.Fatalf("submit should skip cleanly, got %v", err)
	}
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("busy session under reject must not build a loop, got %d", spy.count)
	}
}

// TestTriggerEnqueueBusySessionQueues pins the enqueue branch: a busy target
// under multitask=enqueue skips the fire (no loop build) but restores the
// claimed slot at a bounded backoff — the firing is queued for a later scan,
// not burned until the next cron occurrence. An unclaimed manual FireNow is a
// quiet skip that leaves the schedule untouched.
func TestTriggerEnqueueBusySessionQueues(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	// A target session with an active run (blocked on a gate so it stays active).
	sess, err := rt.CreateSession(context.Background(), userID, "busy enqueue target")
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
	task.Multitask = MultitaskEnqueue
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
	if err := tr.submit(context.Background(), claimed, true); err != nil {
		t.Fatalf("submit should skip cleanly, got %v", err)
	}
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("busy session under enqueue must not build a loop, got %d", spy.count)
	}
	// The claimed slot is restored at a bounded backoff, NOT advanced to the
	// next cron occurrence: next_run_at must sit in (now, now+enqueueBackoff].
	after, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after queue: %v", err)
	}
	ceil := now.Add(enqueueBackoff + time.Minute)
	if !after.NextRunAt.After(now) || after.NextRunAt.After(ceil) {
		t.Fatalf("enqueue task next_run_at = %v, want in (%v, %v]", after.NextRunAt, now, ceil)
	}

	// A manual FireNow against the still-busy session is a quiet skip: it must
	// not rewrite next_run_at (the cadence's slot is left as-is, requeue would
	// fire the task early).
	before, _ := store.Get(context.Background(), created.ID)
	if err := tr.FireNow(context.Background(), before); err != nil {
		t.Fatalf("firenow while busy: %v", err)
	}
	afterNow, _ := store.Get(context.Background(), created.ID)
	if !afterNow.NextRunAt.Equal(before.NextRunAt) {
		t.Fatalf("manual enqueue fire moved next_run_at: %v -> %v", before.NextRunAt, afterNow.NextRunAt)
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
	if err := tr.submit(context.Background(), claimed, true); err != nil {
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

// TestTriggerFireNowSkipDoesNotRequeue pins the FireNow/cron contract: an
// unclaimed manual run skipped by a transient gate (budget exceeded) must NOT
// rewrite next_run_at — requeueing would push it to now and make the next scan
// fire the task early, outside its cadence.
func TestTriggerFireNowSkipDoesNotRequeue(t *testing.T) {
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
	tr.WithBudgetGate(func(ctx context.Context, userID, teamID string) error {
		return quota.ErrBudgetExceeded
	})
	if err := tr.FireNow(context.Background(), created); err != nil {
		t.Fatalf("firenow over budget: %v", err)
	}
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("budget-skipped fire built %d loops, want 0", spy.count)
	}

	// The schedule is untouched: next_run_at is still the cadence's next
	// occurrence, NOT requeued to now (which would fire it on the next scan).
	after, _ := store.Get(context.Background(), created.ID)
	if !after.NextRunAt.Equal(before.NextRunAt) {
		t.Fatalf("manual over-budget fire moved next_run_at: %v -> %v", before.NextRunAt, after.NextRunAt)
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

// TestTriggerBudgetSkipRequeuesForNextScan pins the skip-path contract: a fire
// skipped by the budget gate must NOT burn the slot Claim consumed — next_run_at
// is pushed back to now, so the next scan retries the task (a daily task would
// otherwise wait 24h for its next chance). The gate runs BEFORE the session is
// resolved, so an over-budget FRESH task must not create (and later clean up) a
// tagged session on every scan — the requeue happens with zero session churn.
func TestTriggerBudgetSkipRequeuesForNextScan(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	task := validTask(userID) // no target session: the fire would create one
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })
	t.Cleanup(func() {
		ids, _ := store.ListSessions(context.Background(), created.ID)
		for _, id := range ids {
			db.Exec(`DELETE FROM sessions WHERE id = $1`, id)
		}
	})

	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)
	claimed, err := store.Claim(context.Background(), created.ID, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	tr.WithBudgetGate(func(ctx context.Context, userID, teamID string) error {
		return quota.ErrBudgetExceeded
	})
	if err := tr.submit(context.Background(), claimed, true); err != nil {
		t.Fatalf("submit should skip cleanly, got %v", err)
	}
	// The skip must not build a loop, start a run, or create a session — no
	// churn while the task is over budget.
	if atomic.LoadInt32(&spy.count) != 0 {
		t.Fatalf("budget-skipped fire built %d loops, want 0", spy.count)
	}
	if ids, err := store.ListSessions(context.Background(), created.ID); err != nil || len(ids) != 0 {
		t.Fatalf("budget-skipped fire created sessions %v (err %v), want none", ids, err)
	}

	// The claimed slot was not burned: next_run_at is back at (or before) the
	// moment the requeue ran, so a scan now finds the task due again.
	after, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after skip: %v", err)
	}
	scanNow := time.Now()
	if after.NextRunAt.After(scanNow) {
		t.Fatalf("budget-skipped task next_run_at = %v, want requeued to <= now", after.NextRunAt)
	}
	due, err := store.ListDue(context.Background(), scanNow)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	found := false
	for _, d := range due {
		if d.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Error("budget-skipped task is not due on the next scan")
	}
}

// TestTriggerLoopBuildFailureLeavesClaimed pins the persistent-failure policy:
// a loop build failure (unresolvable provider/model) must NOT requeue — the
// claimed slot stays advanced and the task is due again at the NEXT cron
// occurrence, never every scan. Requeueing a misconfigured task would
// hot-loop the scan (build fails, session churn every 30s) until an operator
// fixes it. The budget-skip path above shows the transient contrast: it IS
// requeued.
func TestTriggerLoopBuildFailureLeavesClaimed(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	created, err := store.Create(context.Background(), validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	now := time.Now()
	db.Exec(`UPDATE scheduled_task SET next_run_at = $1 WHERE id = $2`, now.Add(-time.Minute), created.ID)
	claimed, err := store.Claim(context.Background(), created.ID, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	build := func(ctx context.Context, task Task, system, model string) (*agent.Loop, error) {
		return nil, errors.New("provider cannot be resolved")
	}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, build, db, time.Hour)
	if err := tr.submit(context.Background(), claimed, true); err == nil {
		t.Fatal("a failing loop build must surface as an error")
	}

	// The claimed slot was NOT pushed back: next_run_at is still the claim's
	// future occurrence, so a scan right now does not find the task due (no
	// hot loop) and the next cron slot will retry it.
	after, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after failed fire: %v", err)
	}
	if !after.NextRunAt.After(time.Now()) {
		t.Fatalf("failed fire must leave next_run_at claimed in the future, got %v", after.NextRunAt)
	}
	if !after.NextRunAt.Equal(claimed.NextRunAt) {
		t.Fatalf("failed fire moved next_run_at: claimed %v, now %v", claimed.NextRunAt, after.NextRunAt)
	}
	due, err := store.ListDue(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	for _, d := range due {
		if d.ID == created.ID {
			t.Error("loop-build-failed task leaked back into the due scan set")
		}
	}
}

// dueScopedStore narrows ListDue to a fixed set of task ids, so a sweep test is
// insulated from overdue tasks other tests left in the shared dev database.
// Every other method delegates to the wrapped store.
type dueScopedStore struct {
	Store
	ids map[string]bool
}

// TestTriggerDeletesFreshSessionOnRunCompletion pins on_run_completed = delete
// to the RUN's lifetime: the fresh session is removed only once the run
// settles (done/failed/cancelled) — the RunDoneHook path, not a fixed poll
// window — so a run that outlives the old 30-minute timeout still gets its
// session cleaned up, and an in-flight run keeps its session.
func TestTriggerDeletesFreshSessionOnRunCompletion(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	task := validTask(userID)
	task.OnRunCompleted = OnRunDelete
	created, err := store.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	// Gate the run: the fired run stays in flight until released — the
	// stand-in for a long-running scheduled run.
	gate := make(chan struct{})
	spy := &loopSpy{gate: gate}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	if err := tr.submit(context.Background(), created, false); err != nil {
		t.Fatalf("submit: %v", err)
	}

	ids, err := store.ListSessions(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("want 1 produced session, got %v", ids)
	}
	sessID := ids[0]
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, sessID) })

	// The run is still in flight: the session must NOT be deleted yet.
	if n := countSessions(t, db, sessID); n != 1 {
		t.Fatalf("session count while the run is in flight = %d, want 1", n)
	}

	// Release the run; it settles done and the hook deletes the session.
	close(gate)
	waitForNoSession(t, db, sessID)
}

// TestTriggerKeepsFreshSessionOnCompletion pins on_run_completed = keep (the
// default): a settled run must NOT delete the fresh session.
func TestTriggerKeepsFreshSessionOnCompletion(t *testing.T) {
	db := pgTestDB(t)
	rt, rg, userID := newRuntime(t, db)

	store := NewPGStore(db)
	created, err := store.Create(context.Background(), validTask(userID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { store.Delete(context.Background(), created.ID) })

	spy := &loopSpy{}
	tr := NewTrigger(store, rt, rg, fakeDefs{}, fakeScopes{}, spy.build, db, time.Hour)
	if err := tr.submit(context.Background(), created, false); err != nil {
		t.Fatalf("submit: %v", err)
	}

	ids, err := store.ListSessions(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("want 1 produced session, got %v", ids)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM sessions WHERE id = $1`, ids[0]) })

	// Wait for the run to settle (the stub provider finishes immediately),
	// then the session must still exist.
	waitForSessionSettled(t, db, ids[0])
	if n := countSessions(t, db, ids[0]); n != 1 {
		t.Fatalf("session count after a keep run = %d, want 1", n)
	}
}

func countSessions(t *testing.T, db *sql.DB, sessID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE id = $1`, sessID).Scan(&n); err != nil {
		t.Fatalf("count session: %v", err)
	}
	return n
}

// waitForSessionSettled polls until the session has no active run (the run
// reached a terminal state), so a follow-up assertion sees the post-run state.
func waitForSessionSettled(t *testing.T, db *sql.DB, sessID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := db.QueryRow(`SELECT status FROM runs WHERE session_id = $1 AND status IN ('running','queued')`, sessID).Scan(&status); err == sql.ErrNoRows {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s run did not settle within 10s", sessID)
}

// waitForNoSession polls until the session row is gone (deleted by the
// on_run_completed = delete hook once the run settled).
func waitForNoSession(t *testing.T, db *sql.DB, sessID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countSessions(t, db, sessID) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s still present after its run settled", sessID)
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
