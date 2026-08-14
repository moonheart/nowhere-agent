package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// usageToolScriptProvider is toolScriptProvider with per-call usage reported
// on every stop, so the ledger path has something to record.
type usageToolScriptProvider struct{ calls int }

func (p *usageToolScriptProvider) Name() string { return "usagescript" }

func (p *usageToolScriptProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	p.calls++
	if p.calls == 1 {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "echo", ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{"x":1}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopToolUse,
			Usage: &provider.Usage{InputTokens: 10, OutputTokens: 3}}
	} else {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "done"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn,
			Usage: &provider.Usage{InputTokens: 7, OutputTokens: 2}}
	}
	close(ch)
	return ch, nil
}

// TestRunStepIntentsWrittenBeforeEffects verifies the intent-before-effect
// discipline end to end (change durable-run-accounting): every assistant step
// and every executed tool call writes a durable intent carrying a provisioned
// result id, and the persisted messages land with exactly those ids — assistant
// intents one per message, a parallel batch's tool intents sharing the batch's
// id (their results land in one tool-result message).
func TestRunStepIntentsWrittenBeforeEffects(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	reg := toolruntime.NewRegistry()
	reg.Register(regEchoTool{})
	loop := agent.New(&toolScriptProvider{}, reg, agent.Config{Model: "m", MaxTokens: 100})

	userMsg := provider.TextMessage(provider.RoleUser, "hi")
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop, UserMessage: &userMsg})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Fatalf("status = %v want done", got)
	}

	steps, err := rt.store.LatestRunSteps(context.Background(), run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) < 3 {
		t.Fatalf("got %d step intents, want >= 3 (assistant, tool, assistant)", len(steps))
	}

	msgs, err := ms.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// user + assistant(tool_use) + user(tool_result) + assistant(text)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4", len(msgs))
	}

	var assistantSteps, toolSteps []RunStep
	for _, s := range steps {
		switch s.StepKind {
		case StepAssistant:
			assistantSteps = append(assistantSteps, s)
		case StepTool:
			toolSteps = append(toolSteps, s)
		}
	}
	if len(assistantSteps) != 2 {
		t.Fatalf("assistant intents = %d, want 2", len(assistantSteps))
	}
	if len(toolSteps) != 1 {
		t.Fatalf("tool intents = %d, want 1", len(toolSteps))
	}

	// Durable attempt counts: 1-based, per (run, step_kind). LatestRunSteps
	// returns newest first, so the first assistant step listed is the second
	// call.
	if assistantSteps[0].Attempt != 2 || assistantSteps[1].Attempt != 1 {
		t.Errorf("assistant attempts (newest first) = %d, %d; want 2, 1", assistantSteps[0].Attempt, assistantSteps[1].Attempt)
	}
	if toolSteps[0].Attempt != 1 {
		t.Errorf("tool attempt = %d, want 1", toolSteps[0].Attempt)
	}

	// Provisioned-id binding: assistant message rows carry the intent ids.
	if assistantSteps[0].ResultMessageID == nil || assistantSteps[1].ResultMessageID == nil {
		t.Fatal("assistant intents carry no provisioned result id")
	}
	var assistantIDs = map[int64]bool{
		*assistantSteps[0].ResultMessageID: false,
		*assistantSteps[1].ResultMessageID: false,
	}
	got := 0
	for _, m := range msgs {
		if m.Role == provider.RoleAssistant {
			if seen, ok := assistantIDs[m.ID]; ok && !seen {
				assistantIDs[m.ID] = true
				got++
			}
		}
	}
	if got != 2 {
		t.Errorf("assistant messages bound to intent ids = %d, want 2 (messages %+v)", got, msgs)
	}

	// The tool batch's result message carries the shared tool intent id.
	if toolSteps[0].ResultMessageID == nil {
		t.Fatal("tool intent carries no provisioned result id")
	}
	foundTool := false
	for _, m := range msgs {
		if m.Role == provider.RoleUser && m.ID == *toolSteps[0].ResultMessageID {
			foundTool = true
		}
	}
	if !foundTool {
		t.Errorf("no tool-result message carries the tool intent id %d", *toolSteps[0].ResultMessageID)
	}
}

// TestUsageLedgerMatchesMessagesAndRun verifies the ledger semantics (change
// durable-run-accounting): one assistant-caused record per assistant message,
// bound to the message's id, and the run's aggregate recomputed from the ledger
// equals the sum of the messages' usage.
func TestUsageLedgerMatchesMessagesAndRun(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	reg := toolruntime.NewRegistry()
	reg.Register(regEchoTool{})
	loop := agent.New(&usageToolScriptProvider{}, reg, agent.Config{Model: "m", MaxTokens: 100})

	userMsg := provider.TextMessage(provider.RoleUser, "hi")
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop, UserMessage: &userMsg})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Fatalf("status = %v want done", got)
	}

	// Every assistant message has exactly one ledger record bound to its id;
	// user/tool-result messages have none.
	msgs, err := ms.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantInput, wantOutput := 0, 0
	assistantCount := 0
	for _, m := range msgs {
		if m.Role == provider.RoleAssistant {
			assistantCount++
			if m.Usage != nil {
				wantInput += m.Usage.InputTokens
				wantOutput += m.Usage.OutputTokens
			}
		}
	}
	if assistantCount != 2 {
		t.Fatalf("assistant messages = %d, want 2", assistantCount)
	}

	store := rt.store.(*MemStore)
	records := store.usageRecords[run.ID]
	if len(records) != assistantCount {
		t.Fatalf("usage records = %d, want %d", len(records), assistantCount)
	}
	for _, r := range records {
		if r.Cause != UsageAssistant {
			t.Errorf("record cause = %q, want assistant", r.Cause)
		}
		if r.ResultMessageID == nil {
			t.Error("record not bound to a message id")
			continue
		}
		found := false
		for _, m := range msgs {
			if m.ID == *r.ResultMessageID && m.Role == provider.RoleAssistant {
				found = true
			}
		}
		if !found {
			t.Errorf("record bound to %d, which is not an assistant message", *r.ResultMessageID)
		}
	}

	// Run aggregate recomputed from the ledger equals the message-sum.
	sum, err := store.SumUsage(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.InputTokens != wantInput || sum.OutputTokens != wantOutput {
		t.Errorf("ledger sum = %+v, want input=%d output=%d", sum, wantInput, wantOutput)
	}
}

// TestAdjustmentDoesNotMutateMessageUsage verifies adjustments live in the
// ledger and never touch the messages' immutable usage snapshots.
func TestAdjustmentDoesNotMutateMessageUsage(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	runID := "run-a"

	if err := store.AppendUsageRecord(ctx, UsageRecord{
		RunID: runID, Cause: UsageAssistant,
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendUsageRecord(ctx, UsageRecord{
		RunID: runID, Cause: UsageAdjustment,
		Usage: provider.Usage{InputTokens: -2},
	}); err != nil {
		t.Fatal(err)
	}

	sum, err := store.SumUsage(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.InputTokens != 8 {
		t.Errorf("ledger sum input = %d, want 8 (adjustment included)", sum.InputTokens)
	}
	if sum.OutputTokens != 5 {
		t.Errorf("ledger sum output = %d, want 5", sum.OutputTokens)
	}
}

// overflowUsageProvider truncates (recoverably) on the first call and answers
// fully on the second: the discarded attempt's usage must still reach the
// ledger.
type overflowUsageProvider struct{ calls int }

func (p *overflowUsageProvider) Name() string { return "overflow-usage" }

func (p *overflowUsageProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	p.calls++
	if p.calls == 1 {
		// Recoverable truncation: max_tokens stop below the intended cap.
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "cut"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopMaxTokens,
			Usage: &provider.Usage{InputTokens: 10, OutputTokens: 3}}
	} else {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "done"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn,
			Usage: &provider.Usage{InputTokens: 7, OutputTokens: 2}}
	}
	close(ch)
	return ch, nil
}

// TestUsageLedgerRecordsDiscardedOverflowResponse: the recoverable-truncation
// path discards the truncated response — no message ever persists — but its
// tokens were consumed and reported live (KindUsage accumulates them), so the
// ledger must carry them as an overflow record for the durable aggregate and
// the live frame to agree.
func TestUsageLedgerRecordsDiscardedOverflowResponse(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	loop := agent.New(&overflowUsageProvider{}, toolruntime.NewRegistry(),
		agent.Config{Model: "m", MaxTokens: 100})
	// Enough history for the overflow guard to drop a round (a bare single
	// user message has nothing safe to drop and the run would fail).
	history := []provider.Message{
		provider.TextMessage(provider.RoleUser, "u1"),
		provider.TextMessage(provider.RoleAssistant, "a1"),
		provider.TextMessage(provider.RoleUser, "u2"),
		provider.TextMessage(provider.RoleAssistant, "a2"),
		provider.TextMessage(provider.RoleUser, "hi"),
	}
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop, History: history, UserMessage: &history[4]})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Fatalf("status = %v want done", got)
	}

	store := rt.store.(*MemStore)
	records := store.usageRecords[run.ID]
	if len(records) != 2 {
		t.Fatalf("usage records = %d, want 2 (surviving assistant + overflow)", len(records))
	}
	var overflow, assistant *UsageRecord
	for i := range records {
		r := &records[i]
		switch r.Cause {
		case UsageOverflow:
			overflow = r
		case UsageAssistant:
			assistant = r
		default:
			t.Errorf("unexpected record cause %q", r.Cause)
		}
	}
	if overflow == nil {
		t.Fatal("no overflow usage record for the discarded response")
	}
	if overflow.Attempt != 1 {
		t.Errorf("overflow record attempt = %d, want 1 (the discarded attempt)", overflow.Attempt)
	}
	if overflow.Usage.InputTokens != 10 || overflow.Usage.OutputTokens != 3 {
		t.Errorf("overflow record usage = %+v, want the truncated attempt's 10/3", overflow.Usage)
	}
	if overflow.ResultMessageID != nil {
		t.Error("overflow record must not claim a message id")
	}
	if assistant == nil || assistant.Usage.InputTokens != 7 || assistant.Usage.OutputTokens != 2 {
		t.Errorf("surviving assistant record = %+v, want usage 7/2", assistant)
	}

	// The run aggregate (recomputed from the ledger) includes the discarded
	// attempt — matching the live data-usage frame's accumulated totals.
	sum, err := store.SumUsage(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.InputTokens != 17 || sum.OutputTokens != 5 {
		t.Errorf("ledger sum = %+v, want input=17 output=5 (discarded attempt included)", sum)
	}
}

// TestAppendRunStepAttemptCounters verifies durable per-(run, kind) attempt
// counters: they increment within one run and kind, and restart across runs.
func TestAppendRunStepAttemptCounters(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		st, err := store.AppendRunStep(ctx, "run-1", StepAssistant, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if st.Attempt != i {
			t.Fatalf("attempt = %d, want %d", st.Attempt, i)
		}
		if st.Seq != i {
			t.Fatalf("seq = %d, want %d", st.Seq, i)
		}
		if st.ResultMessageID == nil {
			t.Fatal("assistant intent without provisioned id")
		}
	}
	st, err := store.AppendRunStep(ctx, "run-1", StepTool, "tu-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Attempt != 1 {
		t.Errorf("tool attempt = %d, want 1 (independent of assistant counter)", st.Attempt)
	}

	// A fresh run restarts the counters (the durable state, not process
	// memory, is the source of truth across restarts).
	st, err = store.AppendRunStep(ctx, "run-2", StepAssistant, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Attempt != 1 || st.Seq != 1 {
		t.Errorf("fresh run attempt/seq = %d/%d, want 1/1", st.Attempt, st.Seq)
	}

	// A parallel tool batch shares one provisioned id.
	shared := st.ResultMessageID
	for _, tc := range []string{"tu-2", "tu-3"} {
		ts, err := store.AppendRunStep(ctx, "run-2", StepTool, tc, shared)
		if err != nil {
			t.Fatal(err)
		}
		if ts.ResultMessageID == nil || *ts.ResultMessageID != *shared {
			t.Errorf("shared batch id broken: %v vs %v", ts.ResultMessageID, shared)
		}
	}
}

// TestToolIntentMWParallelBatchProvisionsOnce pins the batch-id race: parallel
// tool dispatch must provision the shared result id exactly ONCE. Two racing
// first-callers used to each read batchID == nil and provision their own id —
// the second intent's id never got a message, so recovery misread the batch as
// an interrupted step. 25 rounds of a 16-way batch: the old race collides
// essentially every round; the fix never can.
func TestToolIntentMWParallelBatchProvisionsOnce(t *testing.T) {
	store := NewMemStore()
	rg := NewRunRegistry(NewRuntime(store))
	ctx := context.Background()
	sess, err := store.CreateSession(ctx, "u1", "t")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	pending := &stepIntentQueue{}
	mw := &toolIntentMW{rg: rg, sessionID: sess.ID, runID: run.ID, pending: pending}

	for round := 0; round < 25; round++ {
		const n = 16
		before, _ := store.LatestRunSteps(context.Background(), run.ID, 1000)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				mw.WrapToolCall(ctx, &agent.ToolCall{
					Call: toolruntime.Call{ID: fmt.Sprintf("tc-%d", i), Name: "echo"},
				}, func(context.Context, *agent.ToolCall) toolruntime.Result {
					return toolruntime.Result{Content: "ok"}
				})
			}(i)
		}
		wg.Wait()

		pending.mu.Lock()
		ids := append([]stepIntent{}, pending.ids...)
		pending.mu.Unlock()
		if len(ids) != n {
			t.Fatalf("round %d: intents = %d, want %d", round, len(ids), n)
		}
		var first *int64
		for _, in := range ids {
			if first == nil {
				first = in.messageID
				continue
			}
			if in.messageID == nil || *in.messageID != *first {
				t.Fatalf("round %d: batch provisioned more than one id (%d vs %d)", round, *first, *in.messageID)
			}
		}
		if first == nil {
			t.Fatalf("round %d: no intent written", round)
		}
		// Exactly one durable step row per call: the deciding caller provisions
		// AND pushes in one go — a second append for the same call would leave
		// a duplicate step row recovery reads as a phantom tool call.
		if steps, _ := store.LatestRunSteps(context.Background(), run.ID, 1000); len(steps)-len(before) != n {
			t.Fatalf("round %d: durable step rows added = %d, want %d (one per call)", round, len(steps)-len(before), n)
		}
		// Fresh batch: the next round provisions a fresh id. Drain the queue
		// the way the emitter's tool-result persist path does.
		mw.resetBatch()
		_ = pending.popTools()
	}
}

// blockingStepStore wraps a MemStore and tracks how many AppendRunStep calls
// are in flight simultaneously, so tests can assert the registry's step lock
// scope: same-session appends serialize, different sessions proceed
// concurrently.
type blockingStepStore struct {
	*MemStore
	mu          sync.Mutex
	inflight    int
	maxInflight int
}

func (b *blockingStepStore) AppendRunStep(ctx context.Context, runID string, kind StepKind, toolCallID string, resultMessageID *int64) (RunStep, error) {
	b.mu.Lock()
	b.inflight++
	if b.inflight > b.maxInflight {
		b.maxInflight = b.inflight
	}
	b.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	defer func() {
		b.mu.Lock()
		b.inflight--
		b.mu.Unlock()
	}()
	return b.MemStore.AppendRunStep(ctx, runID, kind, toolCallID, resultMessageID)
}

func (b *blockingStepStore) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inflight, b.maxInflight = 0, 0
}

// TestAppendStepSerializesPerSessionNotPerProcess pins the lock-scope change:
// the registry's per-session step lock serializes concurrent intents for the
// SAME session (a parallel tool batch races on the run's seq/attempt MAX+1)
// but appends of DIFFERENT sessions overlap — the old process-wide stepMu
// serialized every session's accounting on one mutex.
func TestAppendStepSerializesPerSessionNotPerProcess(t *testing.T) {
	ctx := context.Background()
	blocking := &blockingStepStore{MemStore: NewMemStore()}
	rg := NewRunRegistry(NewRuntime(blocking))

	// Two appends for DIFFERENT sessions must overlap (per-session locks).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = rg.appendStep(ctx, "s1", "r1", StepAssistant, "", nil) }()
	go func() { defer wg.Done(); _, _ = rg.appendStep(ctx, "s2", "r2", StepAssistant, "", nil) }()
	wg.Wait()
	if got := blocking.maxInflight; got < 2 {
		t.Fatalf("different sessions: max concurrent step appends = %d, want >= 2 (lock must not be process-wide)", got)
	}

	// Two appends for the SAME session must serialize (a parallel tool batch
	// within one run).
	blocking.reset()
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = rg.appendStep(ctx, "s1", "r1", StepTool, "tu-a", nil) }()
	go func() { defer wg.Done(); _, _ = rg.appendStep(ctx, "s1", "r1", StepTool, "tu-b", nil) }()
	wg.Wait()
	if got := blocking.maxInflight; got > 1 {
		t.Fatalf("same session: max concurrent step appends = %d, want 1 (serialized)", got)
	}
}
