package chatapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
	"nowhere-agent/internal/toolruntime/builtin"
)

// genUIProvider calls test_ui once, then answers with text.
type genUIProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *genUIProvider) Name() string { return "genui-script" }

func (p *genUIProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	ch := make(chan provider.Event, 8)
	if p.calls == 1 {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "test_ui", ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopToolUse}
	} else {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "card pushed"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	}
	close(ch)
	return ch, nil
}

// TestChatStreamsGenerativeUIFrameLive drives the FULL runtime-wired stack (loop
// → registryEmitter → AppendEvent broker routing → attach → SSE) and asserts the
// data-generative-ui frame reaches the submitting client live. Regression: the
// kind was missing from isContentKind, so the frame fell to the durable bus
// instead of the broker and only a page refresh (history path) showed the UI.
func TestChatStreamsGenerativeUIFrameLive(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	msgStore := session.NewMemMessageStore()
	user := identity.User{ID: "genui-user"}

	h := NewHandler(func(ctx context.Context, system, model string) *agent.Loop {
		reg := toolruntime.NewRegistry()
		reg.Register(builtin.NewTestUI())
		return agent.New(&genUIProvider{}, reg, agent.Config{Model: "m", System: system, MaxTokens: 100})
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore)

	mux := http.NewServeMux()
	h.Register(mux)

	sess, err := rt.CreateSession(context.Background(), user.ID, "genui")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"threadId":"` + sess.ID + `","messages":[{"role":"user","content":"show me the ui"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		`"type":"tool-call-start"`, `"toolName":"test_ui"`, `"type":"tool-result"`,
		// The live UI frame with the spec payload.
		`"type":"data-generative-ui"`, `"test-ui-card"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("live stream missing %q\n%s", want, out)
		}
	}

	// The durable message record must carry the spec block, so a history reload
	// re-renders the card.
	msgs, err := msgStore.MessagesFor(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("MessagesFor: %v", err)
	}
	var sawSpec bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.GenerativeUI != nil {
				sawSpec = true
			}
		}
	}
	if !sawSpec {
		t.Error("durable message store lacks the generative_ui block")
	}
}

// progressUIProvider calls ui_progress once, then answers with text.
type progressUIProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *progressUIProvider) Name() string { return "progress-script" }

func (p *progressUIProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	ch := make(chan provider.Event, 8)
	if p.calls == 1 {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "ui_progress", ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopToolUse}
	} else {
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "progress done"}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	}
	close(ch)
	return ch, nil
}

// TestChatStreamsProgressFramesLive drives the full runtime-wired stack with a
// tool that pushes progress frames mid-call, asserting MULTIPLE
// data-generative-ui frames reach the submitting client live (one per stage,
// plus the durable final spec), so a progress card updates in place.
func TestChatStreamsProgressFramesLive(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	msgStore := session.NewMemMessageStore()
	user := identity.User{ID: "progress-user"}

	h := NewHandler(func(ctx context.Context, system, model string) *agent.Loop {
		reg := toolruntime.NewRegistry()
		reg.Register(builtin.NewProgressUI())
		return agent.New(&progressUIProvider{}, reg, agent.Config{Model: "m", System: system, MaxTokens: 100})
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore)

	mux := http.NewServeMux()
	h.Register(mux)

	sess, err := rt.CreateSession(context.Background(), user.ID, "progress")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"threadId":"` + sess.ID + `","messages":[{"role":"user","content":"show progress"}]}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	n := strings.Count(out, `"type":"data-generative-ui"`)
	// 5 stage pushes + the durable final frame from recordToolResults.
	if n < 4 {
		t.Errorf("got %d data-generative-ui frames, want >= 4 (5 live pushes + final)\n%s", n, out)
	}
	// The frames must carry progressing percents.
	if !strings.Contains(out, `"percent":100`) {
		t.Errorf("live stream missing the final 100%% spec\n%s", out)
	}
}
