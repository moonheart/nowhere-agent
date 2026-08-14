package chatapi

import (
	"context"
	"encoding/json"
	"errors"
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

// flakyFold fails the first fold attempt (a DB/execution failure between the
// decision commit and the fold commit), then performs the real approval
// semantics. It injects the failure mode the recovery path exists for.
type flakyFold struct {
	calls *int
}

func (f flakyFold) Fold(ctx context.Context, in session.Interaction, approve bool, tools *toolruntime.Registry) (toolruntime.Result, error) {
	*f.calls++
	if *f.calls == 1 {
		return toolruntime.Result{}, errors.New("fold boom")
	}
	if !approve {
		return toolruntime.Result{Content: "the user denied permission to run " + in.ToolName, IsError: true}, nil
	}
	var input map[string]any
	_ = json.Unmarshal(in.Payload, &input)
	return tools.CallAll(ctx, []toolruntime.Call{{ID: in.ToolCallID, Name: in.ToolName, Args: input}})[0], nil
}

// dangerCountTool records executions of the gated tool.
type dangerCountTool struct{ runs *int }

func (d dangerCountTool) Name() string           { return "danger" }
func (d dangerCountTool) Description() string    { return "gated" }
func (d dangerCountTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (d dangerCountTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }
func (d dangerCountTool) Timeout() time.Duration { return time.Second }
func (d dangerCountTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	*d.runs++
	return toolruntime.Result{Content: "danger done"}, nil
}

// TestChatResumeRetryAfterFoldFailure pins the recovery path: the verdict's
// decision commits but the fold fails; the client retries the SAME verdict and
// must reach the idempotent fold — not a 409 "already decided" deadlock that
// would leave the approved call unexecuted and its tool_use dangling forever.
func TestChatResumeRetryAfterFoldFailure(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	msgStore := session.NewMemMessageStore()

	dangerRuns := 0
	h := NewHandler(func(ctx context.Context, system, model string) *agent.Loop {
		loop := agent.New(stubProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", System: system, MaxTokens: 100})
		loop.RegisterTool(dangerCountTool{runs: &dangerRuns})
		return loop
	}, "sys").WithRuntime(rt).WithMessageStore(msgStore)

	foldCalls := 0
	h.Registry().RegisterInteractionHandler(session.KindToolApproval, flakyFold{calls: &foldCalls})

	mux := http.NewServeMux()
	h.Register(mux)

	// Turn 1: create the session.
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/chat",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	sessID := store.Sessions()[0].ID

	// Park a suspended batch: the suspending assistant message + snapshot +
	// pending interaction, with the run settled (run-stateless model).
	ctx := context.Background()
	run2, err := store.CreateRun(ctx, sessID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := msgStore.AppendMessage(ctx, session.StoredMessage{
		SessionID: sessID, RunID: run2.ID, Role: provider.RoleAssistant,
		Content: []provider.Block{{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "danger", ToolInput: map[string]any{}}},
	}); err != nil {
		t.Fatal(err)
	}
	ap, err := store.CreateInteractionBatch(ctx, session.SuspendedBatch{
		RunID: run2.ID, SessionID: sessID, ToolCallIDs: []string{"tu1"},
	}, session.Interaction{
		RunID: run2.ID, SessionID: sessID, ToolCallID: "tu1", ToolName: "danger", Kind: session.KindToolApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRunStatus(ctx, run2.ID, session.RunDone); err != nil {
		t.Fatal(err)
	}

	verdict := `{"threadId":"` + sessID + `","approval":{"approvalId":"` + ap.ID + `","approved":true}}`

	// Attempt 1: the fold fails AFTER the decision committed. The batch is NOT
	// folded; the tool never ran. The client must learn the continuation failed
	// (an error frame with a fixed message — the fold's internal error is never
	// streamed), so the verdict stays retriable.
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, httptest.NewRequest("POST", "/api/chat", strings.NewReader(verdict)))
	if !strings.Contains(rec1.Body.String(), `"type":"error"`) {
		t.Fatalf("attempt 1 should surface the fold failure, got %d %s", rec1.Code, rec1.Body.String())
	}
	if strings.Contains(rec1.Body.String(), "fold boom") {
		t.Fatalf("attempt 1 leaked the fold error text, got %d %s", rec1.Code, rec1.Body.String())
	}
	folded, _, err := h.Registry().BatchFoldState(ctx, run2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if folded {
		t.Fatal("batch must not be folded after the fold failure")
	}
	if dangerRuns != 0 {
		t.Fatalf("danger ran %d times on the failed fold, want 0", dangerRuns)
	}

	// Attempt 2 (the retry): the already-decided verdict must reach the
	// idempotent fold — not the 409 "already decided" deadlock.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/chat", strings.NewReader(verdict)))
	if rec2.Code != 200 {
		t.Fatalf("retry status = %d body=%s, want 200", rec2.Code, rec2.Body.String())
	}
	if dangerRuns != 1 {
		t.Errorf("danger ran %d times, want exactly 1 (the approved side effect, once)", dangerRuns)
	}
	folded, _, err = h.Registry().BatchFoldState(ctx, run2.ID)
	if err != nil || !folded {
		t.Errorf("batch should be folded after the retry, folded=%v err=%v", folded, err)
	}
	msgs, err := msgStore.MessagesFor(ctx, sessID)
	if err != nil {
		t.Fatal(err)
	}
	var foundResult bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolResultID == "tu1" {
				foundResult = true
				if b.IsError || b.ToolContent != "danger done" {
					t.Errorf("folded tool_result = %+v, want {tu1, danger done, no error}", b)
				}
			}
		}
	}
	if !foundResult {
		t.Error("the folded tool_result for tu1 was never persisted")
	}

	// Attempt 3 (plain duplicate, fold committed): NOW the 409 applies — a
	// duplicate verdict must not start yet another continuation run.
	runsBefore := len(store.RunsFor(sessID))
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("POST", "/api/chat", strings.NewReader(verdict)))
	if rec3.Code != http.StatusConflict {
		t.Errorf("duplicate verdict after a committed fold: status = %d, want 409", rec3.Code)
	}
	if got := len(store.RunsFor(sessID)); got != runsBefore {
		t.Errorf("duplicate verdict started a run (runs %d → %d)", runsBefore, got)
	}
}
