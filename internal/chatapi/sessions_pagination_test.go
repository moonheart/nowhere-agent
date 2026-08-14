package chatapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
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
	h := NewHandler(func(context.Context, string, string) *agent.Loop { return nil }, "sys").WithRuntime(rt)
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

// TestServeSessionsSearch verifies the q param end to end: the sidebar search
// runs server-side (so old pages are searchable, not just the loaded ones),
// matches case-insensitively, and returns no results for a miss.
func TestServeSessionsSearch(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(func(context.Context, string, string) *agent.Loop { return nil }, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: "searcher"}

	ctx := context.Background()
	for _, title := range []string{"Alpha Plan", "beta report", "spring retro"} {
		if _, err := rt.CreateSession(ctx, user.ID, title); err != nil {
			t.Fatal(err)
		}
	}

	get := func(q string) (titles []string, code int) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/chat/sessions?q="+url.QueryEscape(q), nil)
		req = req.WithContext(identity.NewContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var resp struct {
			Sessions []sessionDTO `json:"sessions"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		for _, s := range resp.Sessions {
			titles = append(titles, s.Title)
		}
		return titles, rec.Code
	}

	if titles, code := get("ALPHA"); code != http.StatusOK || len(titles) != 1 || titles[0] != "Alpha Plan" {
		t.Errorf("q=ALPHA: code=%d titles=%v, want just [Alpha Plan]", code, titles)
	}
	if titles, code := get("retro"); code != http.StatusOK || len(titles) != 1 || titles[0] != "spring retro" {
		t.Errorf("q=retro: code=%d titles=%v, want just [spring retro]", code, titles)
	}
	// A blank q behaves like no filter.
	if titles, code := get("   "); code != http.StatusOK || len(titles) != 3 {
		t.Errorf("q blank: code=%d n=%d, want all 3", code, len(titles))
	}
	if titles, code := get("nope"); code != http.StatusOK || len(titles) != 0 {
		t.Errorf("q=nope: code=%d titles=%v, want none", code, titles)
	}
}

// TestServeSessionsSearchAcrossPages walks a search whose hits span pages: the
// sidebar loads a page at a time, so the q term must travel with the cursor —
// page 2 continues the same filtered set, and non-matching sessions never
// appear on either page (a regression the single-page search test cannot see).
func TestServeSessionsSearchAcrossPages(t *testing.T) {
	store := session.NewMemStore()
	rt := session.NewRuntime(store)
	h := NewHandler(func(context.Context, string, string) *agent.Loop { return nil }, "sys").WithRuntime(rt)
	mux := http.NewServeMux()
	h.Register(mux)
	user := identity.User{ID: "searchpager"}

	ctx := context.Background()
	for _, title := range []string{"needle one", "haystack", "needle two", "misc", "needle three"} {
		if _, err := rt.CreateSession(ctx, user.ID, title); err != nil {
			t.Fatal(err)
		}
	}

	fetch := func(cursor string) (titles []string, next string, code int) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/chat/sessions?q=needle&limit=2&cursor="+cursor, nil)
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
			titles = append(titles, s.Title)
		}
		return titles, resp.NextCursor, rec.Code
	}

	t1, cur, code := fetch("")
	if code != http.StatusOK || len(t1) != 2 || cur == "" {
		t.Fatalf("page 1: code=%d titles=%v cursor=%q", code, t1, cur)
	}
	t2, cur2, code := fetch(cur)
	if code != http.StatusOK || len(t2) != 1 || cur2 != "" {
		t.Fatalf("page 2: code=%d titles=%v cursor=%q", code, t2, cur2)
	}
	// Set equality, not order: the in-memory store ties same-instant created_at
	// and falls back to id ordering, so exact order is not part of the contract
	// here. The contract IS that the q term travels with the cursor: both pages
	// contain only the three matches, exactly once, non-matches never appear.
	got := append(append([]string{}, t1...), t2...)
	sort.Strings(got)
	if strings.Join(got, ",") != "needle one,needle three,needle two" {
		t.Errorf("search walk = %v, want exactly the three matches, no duplicates, no non-matches", got)
	}
}
