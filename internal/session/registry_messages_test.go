package session

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// toolScriptProvider emits a tool_use turn then a final text turn, so a run
// exercises assistant(tool_use) + user(tool_result) + assistant(text).
type toolScriptProvider struct{ calls int }

func (p *toolScriptProvider) Name() string { return "toolscript" }

func (p *toolScriptProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	p.calls++
	if p.calls == 1 {
		// First turn: request a tool call.
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "echo", ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{"x":1}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	} else {
		// Second turn: final text answer.
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "done"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	}
	close(ch)
	return ch, nil
}

type regEchoTool struct{}

func (regEchoTool) Name() string           { return "echo" }
func (regEchoTool) Description() string    { return "echo" }
func (regEchoTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (regEchoTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (regEchoTool) Timeout() time.Duration { return 0 }
func (regEchoTool) Call(_ context.Context, _ map[string]any) (toolruntime.Result, error) {
	return toolruntime.Result{Content: "echo-result"}, nil
}

// TestRunPersistsFullBlockMessages verifies a run persists the user turn, the
// assistant tool_use message, the tool_result message, and the final assistant
// message — all with full blocks — into the MessageStore.
func TestRunPersistsFullBlockMessages(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	ms := NewMemMessageStore()
	rg := NewRunRegistry(rt, rt.Bus()).WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	reg := toolruntime.NewRegistry()
	reg.Register(regEchoTool{})
	loop := agent.New(&toolScriptProvider{}, reg, agent.Config{Model: "m", MaxTokens: 100})

	userMsg := provider.TextMessage(provider.RoleUser, "hi there")
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop, UserMessage: &userMsg})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitSettle(t, rt, sess.ID); got != RunDone {
		t.Fatalf("status = %v want done (run %s)", got, run.ID)
	}

	msgs, err := ms.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// user(input) + assistant(tool_use) + user(tool_result) + assistant(text)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}

	// seq monotonic 0..3
	for i, m := range msgs {
		if m.Seq != i {
			t.Errorf("msg %d seq = %d", i, m.Seq)
		}
	}

	if msgs[0].Role != provider.RoleUser || msgs[0].Content[0].Text != "hi there" {
		t.Errorf("msg0 (user input) wrong: %+v", msgs[0])
	}
	if msgs[1].Role != provider.RoleAssistant || msgs[1].Content[0].Type != provider.BlockToolUse || msgs[1].Content[0].ToolName != "echo" {
		t.Errorf("msg1 (assistant tool_use) wrong: %+v", msgs[1])
	}
	if msgs[2].Role != provider.RoleUser || msgs[2].Content[0].Type != provider.BlockToolResult || msgs[2].Content[0].ToolContent != "echo-result" {
		t.Errorf("msg2 (tool_result) wrong: %+v", msgs[2])
	}
	if msgs[3].Role != provider.RoleAssistant || msgs[3].Content[0].Text != "done" {
		t.Errorf("msg3 (assistant text) wrong: %+v", msgs[3])
	}
}
