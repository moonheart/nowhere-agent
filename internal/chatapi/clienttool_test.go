package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// clientToolScriptProvider is stateless: it inspects the request. When the
// history has a dangling get_clipboard tool_use (no tool_result yet — turn 1)
// it calls the client tool, which the loop must suspend (not execute). When the
// tool_result is present (post-resume) it answers by echoing the clipboard text.
type clientToolScriptProvider struct{}

func (p *clientToolScriptProvider) Name() string { return "client-tool-script" }

func (p *clientToolScriptProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	var clipboard string
	answered := false
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolResultID == "tu1" {
				answered = true
				clipboard = b.ToolContent
			}
		}
	}
	if !answered {
		// Call the client tool; the loop must suspend (not execute).
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "get_clipboard", ToolInput: map[string]any{}}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{}`}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	} else {
		// After resume: echo the clipboard text the client returned.
		ch <- provider.Event{Type: provider.EventMessageStart}
		ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
		ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "clipboard was: " + clipboard}
		ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
		ch <- provider.Event{Type: provider.EventMessageStop}
	}
	close(ch)
	return ch, nil
}

// TestChatClientToolEndToEnd drives the general interrupt for a client-declared
// tool: the model calls get_clipboard (declared in the request body, executed by
// the client), the run suspends with a data-interaction frame of kind
// client_tool, the client POSTs the output as the verdict, and the resumed run
// folds the validated output as the tool result.
func TestChatClientToolEndToEnd(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	msgStore := session.NewMemMessageStore()

	h := NewHandler(func(ctx context.Context, system string) *agent.Loop {
		return agent.New(&clientToolScriptProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore)

	mux := http.NewServeMux()
	h.Register(mux)

	// Turn 1: declare the client tool and let the model call it. The run must
	// suspend (not execute) with a client_tool data-interaction frame.
	decl, _ := json.Marshal(map[string]any{
		"description":  "Read the user's clipboard.",
		"inputSchema":  map[string]any{"type": "object"},
		"outputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
	})
	body := `{"messages":[{"role":"user","content":"what is on my clipboard?"}],"tools":{"get_clipboard":` + string(decl) + `}}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("turn1 status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	// The client tool must NOT have run server-side: no tool-result for it in the
	// stream. (The data-interaction frame that drives the client card is emitted
	// via the broker and can be outrun by a fast in-process run — it's covered by
	// the loop-level TestLoopEndsOnClientTool; here we assert the suspend through
	// the durable pending interaction below.)
	if strings.Contains(out, `"type":"tool-result"`) {
		t.Errorf("client tool must not execute server-side\n---\n%s", out)
	}

	// The run parked a pending client_tool interaction (the authoritative suspend
	// signal a reloading client reads to re-render the card).
	sessID := store.Sessions()[0].ID
	pending, ok, err := h.Registry().PendingApprovalForSession(context.Background(), sessID)
	if err != nil || !ok {
		t.Fatalf("expected a pending interaction, ok=%v err=%v", ok, err)
	}
	if pending.Kind != session.KindClientTool {
		t.Fatalf("pending kind = %q, want client_tool", pending.Kind)
	}

	// Turn 2 (resume): the client POSTs the clipboard output as the verdict. The
	// handler folds it (validated against the declared output schema) and streams
	// the continuation, which echoes the clipboard text.
	resumeBody := `{"threadId":"` + sessID + `","approval":{"approvalId":"` + pending.ID + `","approved":true,"answer":{"output":{"text":"hello clipboard"}}},` +
		`"tools":{"get_clipboard":` + string(decl) + `}}`
	req2 := httptest.NewRequest("POST", "/api/chat", strings.NewReader(resumeBody))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("resume status = %d body=%s", rec2.Code, rec2.Body.String())
	}

	// The durable record is authoritative: the folded tool_result carries the
	// client output (not an error), and the resumed assistant answer echoes it.
	msgs, err := msgStore.MessagesFor(context.Background(), sessID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	var resultText, assistantText string
	var resultIsErr bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolResultID == "tu1" {
				resultText, resultIsErr = b.ToolContent, b.IsError
			}
			if m.Role == provider.RoleAssistant && b.Type == provider.BlockText {
				assistantText = b.Text // last assistant text wins
			}
		}
	}
	if resultIsErr {
		t.Errorf("valid client output should fold non-error, got %q", resultText)
	}
	if !strings.Contains(resultText, "hello clipboard") {
		t.Errorf("folded tool_result = %q, want the client output", resultText)
	}
	if !strings.Contains(assistantText, "hello clipboard") {
		t.Errorf("assistant answer = %q, want it to echo the clipboard text", assistantText)
	}
}

// TestChatClientToolOutputValidated: a client output that violates the declared
// output schema folds as an is_error result (not trusted blindly).
func TestChatClientToolOutputValidated(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	msgStore := session.NewMemMessageStore()
	h := NewHandler(func(ctx context.Context, system string) *agent.Loop {
		return agent.New(&clientToolScriptProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore)
	mux := http.NewServeMux()
	h.Register(mux)

	decl, _ := json.Marshal(map[string]any{
		"description":  "Read the user's clipboard.",
		"inputSchema":  map[string]any{"type": "object"},
		"outputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
	})
	body := `{"messages":[{"role":"user","content":"clip?"}],"tools":{"get_clipboard":` + string(decl) + `}}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/chat", strings.NewReader(body)))
	sessID := store.Sessions()[0].ID
	pending, ok, _ := h.Registry().PendingApprovalForSession(context.Background(), sessID)
	if !ok {
		t.Fatal("no pending interaction")
	}

	// Resume with an output missing the required "text" property.
	resumeBody := `{"threadId":"` + sessID + `","approval":{"approvalId":"` + pending.ID + `","approved":true,"answer":{"output":{"wrong":1}}},` +
		`"tools":{"get_clipboard":` + string(decl) + `}}`
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/chat", strings.NewReader(resumeBody)))
	if rec2.Code != 200 {
		t.Fatalf("resume status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	msgs, _ := msgStore.MessagesFor(context.Background(), sessID)
	var sawErr bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolResultID == "tu1" && b.IsError {
				sawErr = true
			}
		}
	}
	if !sawErr {
		t.Error("schema-violating client output should fold as an is_error tool_result")
	}
}
