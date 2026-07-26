package chatapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// TestChatApprovalResumeStreamsContinuation pins the "reuse the chat endpoint"
// design: a POST /api/chat carrying an `approval` verdict resumes the parked run
// and replies with the SAME ui-message-stream a normal turn uses (start →
// run-status → clean [DONE]), not a one-shot JSON. The run has no live content
// (its provider already produced the gated turn), so the stream is framing only.
func TestChatApprovalResumeStreamsContinuation(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(gatedLoop(newGatedProvider()), "sys").WithRuntime(rt)
	// The parked map is empty (as after a restart), so Resume rebuilds the loop
	// via the LoopSource. Give it one that streams a continuation line, proving
	// the verdict's response carries the run's live output.
	h.Registry().WithLoopSource(func(ctx context.Context, sessionID string) (*agent.Loop, error) {
		return agent.New(stubProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100}), nil
	})
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	sess, err := rt.CreateSession(context.Background(), owner.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	// Park a run: waiting_approval with a durable pending approval.
	run, err := store.CreateRun(context.Background(), sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRunStatus(context.Background(), run.ID, session.RunWaitingApproval); err != nil {
		t.Fatal(err)
	}
	ap, err := store.CreateApproval(context.Background(), session.Approval{
		RunID: run.ID, SessionID: sess.ID, ToolCallID: "tu1", ToolName: "ask_user",
		ToolInput: []byte(`{"questions":[]}`), Kind: "ask_user",
	})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"threadId":"` + sess.ID + `","approval":{"approvalId":"` + ap.ID + `","approved":false}}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("approval resume stream did not terminate")
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"start"`) {
		t.Errorf("missing ui-message-stream start frame\n---\n%s", out)
	}
	if !strings.Contains(out, `"status":"running"`) {
		t.Errorf("missing run-status running frame\n---\n%s", out)
	}
	if !strings.Contains(out, `"delta":"Hi there"`) {
		t.Errorf("missing the resumed run's streamed content\n---\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing clean stream termination\n---\n%s", out)
	}

	// The verdict was recorded and the run resumed to completion (no content).
	if got, _ := store.GetApproval(context.Background(), ap.ID); got.Status != session.ApprovalRejected {
		t.Errorf("approval status = %v, want rejected (skip)", got.Status)
	}
	runs, _ := rt.RunsForSession(context.Background(), sess.ID)
	if runs[len(runs)-1].Status != session.RunDone {
		t.Errorf("run status = %v, want done", runs[len(runs)-1].Status)
	}
}

// TestChatApprovalRejectsUnknown pins the error mapping on the reused endpoint:
// an unknown/already-decided approval id is a conflict, not a stream.
func TestChatApprovalRejectsUnknown(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(gatedLoop(newGatedProvider()), "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	owner := identity.User{ID: "owner"}

	body := `{"threadId":"x","approval":{"approvalId":"no-such","approved":true}}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req = req.WithContext(identity.NewContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
