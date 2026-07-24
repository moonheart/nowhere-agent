package chatapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/subagent"
	"nowhere-agent/internal/toolruntime"
)

// spawnParentProvider drives the parent run: turn 1 calls spawn_agent, turn 2
// answers with text.
type spawnParentProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *spawnParentProvider) Name() string { return "parent" }
func (p *spawnParentProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	ch := make(chan provider.Event, 8)
	if p.calls == 1 {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "sp1", ToolName: subagent.ToolName, ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{"prompt":"research X","description":"do research"}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	} else {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "parent answer"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	}
	close(ch)
	return ch, nil
}

// subChildProvider yields a single text answer (the subagent's finding).
type subChildProvider struct{ text string }

func (subChildProvider) Name() string { return "child" }
func (p subChildProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: p.text}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop}
	close(ch)
	return ch, nil
}

// TestChatSpawnsSubagent drives a full chat run whose parent calls spawn_agent
// (registered via the ToolBinder, exactly as cmd/server wires it), and asserts
// the child's collapsed result lands as the parent's tool_result and the parent
// finishes with its own answer — the subagent path exercised end to end through
// the HTTP handler and durable message record.
func TestChatSpawnsSubagent(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	msgStore := session.NewMemMessageStore()

	subStore := agentdef.NewStore()
	subFactory := func(_ context.Context, def agentdef.AgentDef, _ int) *agent.Loop {
		return agent.New(subChildProvider{"child findings: 42"}, toolruntime.NewRegistry(),
			agent.Config{Model: "m", System: def.System, MaxTokens: 100})
	}

	h := NewHandler(func(ctx context.Context, system string) *agent.Loop {
		return agent.New(&spawnParentProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore).WithToolBinder(func(ctx context.Context, loop *agent.Loop, sessionID string) {
		reg := toolruntime.NewRegistry()
		reg.Register(subagent.NewSpawnTool(subStore, reg, subFactory, 3))
		loop.WithTools(reg)
	})

	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"messages":[{"role":"user","content":"delegate some research"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	if len(store.Sessions()) != 1 {
		t.Fatalf("expected 1 session, got %d", len(store.Sessions()))
	}
	sessID := store.Sessions()[0].ID
	runs := store.RunsFor(sessID)
	if len(runs) != 1 || runs[0].Status != session.RunDone {
		t.Fatalf("run = %+v", runs)
	}

	msgs, err := msgStore.MessagesFor(context.Background(), sessID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	var sawSpawn, sawChildResult bool
	var finalText string
	for _, m := range msgs {
		for _, blk := range m.Content {
			switch blk.Type {
			case provider.BlockToolUse:
				if blk.ToolName == subagent.ToolName {
					sawSpawn = true
				}
			case provider.BlockToolResult:
				if strings.Contains(blk.ToolContent, "child findings: 42") {
					sawChildResult = true
				}
			case provider.BlockText:
				if blk.Text != "" {
					finalText = blk.Text
				}
			}
		}
	}
	if !sawSpawn {
		t.Error("parent did not persist a spawn_agent tool use")
	}
	if !sawChildResult {
		t.Error("child's collapsed result did not land in the parent tool_result")
	}
	if finalText != "parent answer" {
		t.Errorf("final text = %q, want %q", finalText, "parent answer")
	}

	// The child's progress surfaces as live data-subagent frames tagged with the
	// spawn_agent tool-call id (sp1), so the chat UI can nest output under the
	// right card. The broker is best-effort for a fast in-process run (later
	// frames can be dropped when the run outruns the attach), so assert the
	// leading start frame — which carries the toolCallId link — rather than the
	// full stream. The stream-forwarding itself is unit-tested in subagent.
	stream := rec.Body.String()
	for _, want := range []string{
		`"type":"data-subagent"`,
		`"phase":"start"`,
		`"toolCallId":"sp1"`,
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("live stream missing %q\n---\n%s", want, stream)
		}
	}
}
