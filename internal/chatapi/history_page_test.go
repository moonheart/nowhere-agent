package chatapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// TestHistoryPagination pins the ?limit=&before= contract: a bounded request
// returns the NEWEST limit messages (hasMore when older ones exist), the
// before cursor pages backwards, and the legacy unbounded request (no limit)
// still returns everything with hasMore=false.
func TestHistoryPagination(t *testing.T) {
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
	var ids []int64
	for i := 0; i < 5; i++ {
		m, err := ms.AppendMessage(context.Background(), session.StoredMessage{
			SessionID: sess.ID, RunID: "r1", Role: provider.RoleUser,
			Content: []provider.Block{{Type: provider.BlockText, Text: "m" + string(rune('0'+i))}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}

	get := func(q string) (msgs []historyMessage, hasMore bool) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/chat/history?threadId="+sess.ID+q, nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Messages []historyMessage `json:"messages"`
			HasMore  bool             `json:"hasMore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Messages, body.HasMore
	}

	// Tail page: the newest 2 of 5, truncated.
	msgs, hasMore := get("&limit=2")
	if len(msgs) != 2 || msgs[0].Content[0].Text != "m3" || msgs[1].Content[0].Text != "m4" {
		t.Fatalf("tail page = %+v", msgs)
	}
	if !hasMore {
		t.Error("hasMore = false, want true (5 messages, limit 2)")
	}

	// Second page: everything before the first page's head.
	msgs, hasMore = get("&limit=2&before="+fmt.Sprint(ids[3]))
	if len(msgs) != 2 || msgs[0].Content[0].Text != "m1" || msgs[1].Content[0].Text != "m2" {
		t.Fatalf("second page = %+v", msgs)
	}
	if !hasMore {
		t.Error("hasMore = false, want true (m0 still older)")
	}

	// Final page drains.
	msgs, hasMore = get("&limit=2&before="+fmt.Sprint(ids[1]))
	if len(msgs) != 1 || msgs[0].Content[0].Text != "m0" {
		t.Fatalf("final page = %+v", msgs)
	}
	if hasMore {
		t.Error("hasMore = true on the final page")
	}

	// A limit above the conversation size is not truncated.
	msgs, hasMore = get("&limit=10")
	if len(msgs) != 5 || hasMore {
		t.Fatalf("oversized limit: %d msgs, hasMore=%v", len(msgs), hasMore)
	}

	// Legacy: no limit = full conversation, hasMore false.
	msgs, hasMore = get("")
	if len(msgs) != 5 || hasMore {
		t.Fatalf("legacy full load: %d msgs, hasMore=%v", len(msgs), hasMore)
	}
}
