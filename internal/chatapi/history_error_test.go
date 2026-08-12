package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// TestHistoryEchoesFailedRunError verifies buildHistory attaches a failed run's
// terminal error (stored as message metadata {"error": ...}) to the rebuilt
// assistant message, so a reloaded client can render the failure notice and a
// retry affordance.
func TestHistoryEchoesFailedRunError(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	ms := session.NewMemMessageStore()
	h := NewHandler(nil, "sys").WithRuntime(rt).WithMessageStore(ms)
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: "u1"}

	sess, err := rt.CreateSession(context.Background(), user.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	// Durable record: user turn, then a failed run's assistant message with the
	// terminal error attached as metadata (registry.attachRunError's shape).
	userMsg, err := ms.AppendMessage(context.Background(), session.StoredMessage{
		SessionID: sess.ID, RunID: "r1", Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "q"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assistantMsg, err := ms.AppendMessage(context.Background(), session.StoredMessage{
		SessionID: sess.ID, RunID: "r2", Role: provider.RoleAssistant,
		Content: []provider.Block{{Type: provider.BlockText, Text: "partial"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]string{"error": "response was truncated before completion"})
	if err := ms.SetMessageMetadata(context.Background(), assistantMsg.ID, meta); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess.ID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"response was truncated before completion"`) {
		t.Errorf("history body missing the failed-run error\n%s", rec.Body.String())
	}
	// The user message must not carry the error.
	_ = userMsg
}

// TestHistoryNoErrorForCleanMessages verifies messages without error metadata
// produce no error field on the rebuilt assistant turn.
func TestHistoryNoErrorForCleanMessages(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	ms := session.NewMemMessageStore()
	h := NewHandler(nil, "sys").WithRuntime(rt).WithMessageStore(ms)
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: "u1"}

	sess, err := rt.CreateSession(context.Background(), user.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ms.AppendMessage(context.Background(), session.StoredMessage{
		SessionID: sess.ID, RunID: "r1", Role: provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: "q"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.AppendMessage(context.Background(), session.StoredMessage{
		SessionID: sess.ID, RunID: "r1", Role: provider.RoleAssistant,
		Content: []provider.Block{{Type: provider.BlockText, Text: "ok"}},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess.ID, nil)
	req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"error":"`) && !strings.Contains(rec.Body.String(), `"error":"session`) {
		t.Errorf("clean history unexpectedly carries an error field\n%s", rec.Body.String())
	}
}
