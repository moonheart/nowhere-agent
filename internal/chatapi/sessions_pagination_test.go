package chatapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
)

// TestServeSessionsPagination walks the sessions endpoint page by page: the
// sidebar loads 25 at a time (server default), follows nextCursor for the rest,
// and stops when the cursor goes empty. Pages must be disjoint and cover every
// session.
func TestServeSessionsPagination(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(func(context.Context, string) *agent.Loop { return nil }, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: "paging"}

	ctx := context.Background()
	for i := 0; i < 30; i++ {
		if _, err := rt.CreateSession(ctx, user.ID, fmt.Sprintf("chat %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	fetch := func(limit, cursor string) (ids []string, next string, code int) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/chat/sessions?limit="+limit+"&cursor="+cursor, nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var resp struct {
			Sessions   []sessionDTO `json:"sessions"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		for _, s := range resp.Sessions {
			ids = append(ids, s.ID)
		}
		return ids, resp.NextCursor, rec.Code
	}

	// Page 1: default page size (25) with 30 sessions -> 25 + a cursor.
	ids1, cur, code := fetch("", "")
	if code != http.StatusOK || len(ids1) != 25 || cur == "" {
		t.Fatalf("page 1: code=%d n=%d cursor=%q", code, len(ids1), cur)
	}

	// Page 2: the remaining 5, then the list is exhausted.
	ids2, cur2, code := fetch("", cur)
	if code != http.StatusOK || len(ids2) != 5 || cur2 != "" {
		t.Fatalf("page 2: code=%d n=%d cursor=%q", code, len(ids2), cur2)
	}

	seen := map[string]bool{}
	for _, id := range append(ids1, ids2...) {
		if seen[id] {
			t.Errorf("duplicate session %s across pages", id)
		}
		seen[id] = true
	}
	if len(seen) != 30 {
		t.Errorf("walked %d unique sessions, want 30", len(seen))
	}

	// Explicit limit is honored; a malformed cursor is rejected.
	if ids, _, code := fetch("5", ""); code != http.StatusOK || len(ids) != 5 {
		t.Errorf("limit=5: code=%d n=%d", code, len(ids))
	}
	if _, _, code := fetch("", "garbage"); code != http.StatusBadRequest {
		t.Errorf("bad cursor: code=%d want 400", code)
	}
}
