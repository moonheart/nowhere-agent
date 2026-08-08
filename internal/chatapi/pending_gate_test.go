package chatapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/session"
)

// TestChatSubmitBlockedByPendingInteraction pins the pending-interaction gate
// (capability suspend-batch-snapshot): a session with an undecided interaction
// rejects new chat submissions with 409 pending_interaction; once the batch is
// decided and folded, submissions flow again.
func TestChatSubmitBlockedByPendingInteraction(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(newTestLoop, "sys").WithRuntime(rt)

	mux := http.NewServeMux()
	h.Register(mux)

	// Create a session via a first request, then park a pending interaction in
	// it (the state an approval card leaves behind).
	first := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	mux.ServeHTTP(httptest.NewRecorder(), first)
	if len(store.Sessions()) != 1 {
		t.Fatalf("expected 1 session, got %d", len(store.Sessions()))
	}
	sessID := store.Sessions()[0].ID
	run, err := store.CreateRun(context.Background(), sessID, 2)
	if err != nil {
		t.Fatal(err)
	}
	ap, err := store.CreateApproval(context.Background(), session.Interaction{
		RunID: run.ID, SessionID: sessID, ToolCallID: "tu1", ToolName: "danger", Kind: session.KindToolApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The suspended run settles done (run-stateless model); only the pending
	// interaction remains.
	if err := store.UpdateRunStatus(context.Background(), run.ID, session.RunDone); err != nil {
		t.Fatal(err)
	}

	// A new message while the interaction hangs: 409 pending_interaction, no run.
	body := `{"threadId":"` + sessID + `","messages":[{"role":"user","content":"while pending"}]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/chat", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pending_interaction") {
		t.Errorf("body = %s, want the typed pending_interaction error", rec.Body.String())
	}
	if runs := store.RunsFor(sessID); len(runs) != 2 {
		t.Errorf("a blocked submission must not start a run; runs = %d, want 2", len(runs))
	}

	// Decide the interaction (the batch resolves) → submissions flow again.
	if _, err := store.DecideApproval(context.Background(), ap.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/chat", strings.NewReader(body)))
	if rec2.Code != 200 {
		t.Fatalf("after decision status = %d body=%s, want 200", rec2.Code, rec2.Body.String())
	}
}
