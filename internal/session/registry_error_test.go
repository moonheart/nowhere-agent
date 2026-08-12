package session

import (
	"context"
	"encoding/json"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// truncatingProvider emits one partial text turn and then reports an
// unclassifiable max_tokens stop (no usage), which the loop's truncation guard
// turns into a terminal KindError — the classic mid-answer failure.
type truncatingProvider struct{}

func (p *truncatingProvider) Name() string { return "truncating" }

func (p *truncatingProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "partial answer "}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopMaxTokens}
	close(ch)
	return ch, nil
}

// TestRunFailedAttachesErrorMetadataToLastAssistantMessage verifies a failed
// run's terminal error text lands on its last assistant message as metadata
// {"error": ...}, so /history can echo it to a reloaded client (the error used
// to live only in run_events, which history rebuild never reads).
func TestRunFailedAttachesErrorMetadataToLastAssistantMessage(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt, rt.Bus()).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	loop := agent.New(&truncatingProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100})
	userMsg := provider.TextMessage(provider.RoleUser, "hi")
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop, UserMessage: &userMsg})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunFailed {
		t.Fatalf("status = %v want failed (run %s)", got, run.ID)
	}

	msgs, err := ms.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// user(input) + assistant(partial text)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	last := msgs[1]
	if last.Role != provider.RoleAssistant || last.RunID != run.ID {
		t.Fatalf("last message = %+v, want the run's assistant message", last)
	}
	var meta struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(last.Metadata, &meta); err != nil {
		t.Fatalf("metadata = %q, want a JSON object: %v", last.Metadata, err)
	}
	if meta.Error == "" {
		t.Fatalf("metadata = %q, want a non-empty error text", last.Metadata)
	}
	if meta.Error != "response was truncated before completion" {
		t.Errorf("error text = %q, want the loop's truncation error", meta.Error)
	}
	// The user turn must not carry the failure marker.
	if len(msgs[0].Metadata) != 0 {
		t.Errorf("user message metadata = %q, want none", msgs[0].Metadata)
	}
}

// TestRunErrorMetadataScopedToRun verifies the error attaches to the FAILED
// run's assistant message, never to an earlier run's message in the same
// session.
func TestRunErrorMetadataScopedToRun(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt, rt.Bus()).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	// Run 1: a clean run (one assistant turn).
	okLoop := agent.New(&scriptProviderForRegistry{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100})
	okUser := provider.TextMessage(provider.RoleUser, "first")
	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: okLoop, UserMessage: &okUser}); err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Fatalf("run 1 status = %v want done", got)
	}

	// Run 2: the failing run.
	badLoop := agent.New(&truncatingProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100})
	badUser := provider.TextMessage(provider.RoleUser, "second")
	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: badLoop, UserMessage: &badUser}); err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunFailed {
		t.Fatalf("run 2 status = %v want failed", got)
	}

	msgs, err := ms.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4", len(msgs))
	}
	// Only the second run's assistant message carries the error.
	for i, m := range msgs {
		if m.Role != provider.RoleAssistant {
			continue
		}
		if i == 2 && len(m.Metadata) == 0 {
			t.Errorf("run 2 assistant message missing error metadata")
		}
		if i == 0 && len(m.Metadata) != 0 {
			t.Errorf("run 1 assistant message has unexpected metadata %q", m.Metadata)
		}
	}
}

// scriptProviderForRegistry emits one plain text turn, for a clean done run.
type scriptProviderForRegistry struct{}

func (p *scriptProviderForRegistry) Name() string { return "script" }

func (p *scriptProviderForRegistry) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "ok"}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	close(ch)
	return ch, nil
}

// TestRunFailedWithoutAssistantMessageLeavesNoMetadata verifies a run that
// fails BEFORE any output (a provider error on the first call) is tolerated:
// there is no assistant message to attach to, and the run still settles failed.
type errorProvider struct{}

func (p *errorProvider) Name() string { return "err" }

func (p *errorProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 1)
	ch <- provider.Event{Type: provider.EventError, Err: context.DeadlineExceeded}
	close(ch)
	return ch, nil
}

func TestRunFailedWithoutAssistantMessageLeavesNoMetadata(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt, rt.Bus()).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	loop := agent.New(&errorProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100})
	userMsg := provider.TextMessage(provider.RoleUser, "hi")
	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop, UserMessage: &userMsg}); err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunFailed {
		t.Fatalf("status = %v want failed", got)
	}

	msgs, err := ms.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (user only)", len(msgs))
	}
	if len(msgs[0].Metadata) != 0 {
		t.Errorf("user message metadata = %q, want none", msgs[0].Metadata)
	}
}

// TestRunCancelledLeavesNoErrorMetadata verifies a cancelled run is not a
// failure: no error marker is attached (an intentional stop is not retryable).
func TestRunCancelledLeavesNoErrorMetadata(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt, rt.Bus()).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	p := newGatedRegistryProvider()
	loop := agent.New(p, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100})
	userMsg := provider.TextMessage(provider.RoleUser, "hi")
	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop, UserMessage: &userMsg}); err != nil {
		t.Fatal(err)
	}
	// Wait until the provider stream is open, then cancel the run.
	<-p.started
	rg.Cancel(sess.ID)
	if got := waitSettle(t, rt, sess.ID); got != RunCancelled {
		t.Fatalf("status = %v want cancelled", got)
	}

	msgs, err := ms.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if len(m.Metadata) != 0 {
			t.Errorf("message %d metadata = %q, want none after a cancel", m.ID, m.Metadata)
		}
	}
}

// gatedRegistryProvider blocks on a stream until cancelled, mirroring
// gatedProvider in the chatapi tests.
type gatedRegistryProvider struct {
	started chan struct{}
	once    bool
}

func newGatedRegistryProvider() *gatedRegistryProvider {
	return &gatedRegistryProvider{started: make(chan struct{})}
}

func (p *gatedRegistryProvider) Name() string { return "gated" }

func (p *gatedRegistryProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	if !p.once {
		p.once = true
		close(p.started)
	}
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	<-ctx.Done()
	close(ch)
	return ch, nil
}
