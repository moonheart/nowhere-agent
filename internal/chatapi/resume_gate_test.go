package chatapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// helperSiblingTool is the un-gated sibling the fold re-authorizes.
type helperSiblingTool struct{ runs *int }

func (h helperSiblingTool) Name() string           { return "helper" }
func (h helperSiblingTool) Description() string    { return "ungated sibling" }
func (h helperSiblingTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (h helperSiblingTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }
func (h helperSiblingTool) Timeout() time.Duration { return 0 }
func (h helperSiblingTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	*h.runs++
	return toolruntime.Result{Content: "helper done"}, nil
}

// TestChatResumeFoldGateSeesSessionID: the fold re-applies the loop's
// execution gate to un-gated siblings, and the gate resolves the session's
// permission mode from the context's session id. The request ctx does not
// carry that id (only run ctxs do), so serveChatResume must stamp it — or the
// gate's mode lookup silently degrades to the default and an allow_all-mode
// session's sibling would be denied at fold.
func TestChatResumeFoldGateSeesSessionID(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	msgStore := session.NewMemMessageStore()

	helperRuns := 0
	var gateSawSession string
	h := NewHandler(func(ctx context.Context, system string) *agent.Loop {
		loop := agent.New(stubProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
		loop.RegisterTool(dangerCountTool{runs: new(int)})
		loop.RegisterTool(helperSiblingTool{runs: &helperRuns})
		loop.Use(&agent.PermissionMW{Check: func(ctx context.Context, tool toolruntime.Tool) (bool, string) {
			if tool.Name() == "helper" {
				gateSawSession = agent.SessionIDFromContext(ctx)
				return false, ""
			}
			return true, agent.ApprovalReasonPrefix + "ask"
		}})
		return loop
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore)

	mux := http.NewServeMux()
	h.Register(mux)

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/chat",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	sessID := store.Sessions()[0].ID

	ctx := context.Background()
	run2, err := store.CreateRun(ctx, sessID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := msgStore.AppendMessage(ctx, session.StoredMessage{
		SessionID: sessID, RunID: run2.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{
			{Type: provider.BlockToolUse, ToolUseID: "tu_g", ToolName: "danger", ToolInput: map[string]any{}},
			{Type: provider.BlockToolUse, ToolUseID: "tu_h", ToolName: "helper", ToolInput: map[string]any{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ap, err := store.CreateInteractionBatch(ctx, session.SuspendedBatch{
		RunID: run2.ID, SessionID: sessID, ToolCallIDs: []string{"tu_g", "tu_h"},
	}, session.Interaction{
		RunID: run2.ID, SessionID: sessID, ToolCallID: "tu_g", ToolName: "danger", Kind: session.KindToolApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRunStatus(ctx, run2.ID, session.RunDone); err != nil {
		t.Fatal(err)
	}

	verdict := `{"threadId":"` + sessID + `","approval":{"approvalId":"` + ap.ID + `","approved":true}}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/chat", strings.NewReader(verdict)))
	if rec.Code != 200 {
		t.Fatalf("resume status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if helperRuns != 1 {
		t.Errorf("helper ran %d times, want 1 (allowed sibling dispatched at fold)", helperRuns)
	}
	if gateSawSession != sessID {
		t.Errorf("gate saw session id %q, want %q — the fold ctx must carry the session id for the gate's mode resolution", gateSawSession, sessID)
	}
}
