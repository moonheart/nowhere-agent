package inbound

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// env builds the full HTTP surface: PG store + mem runtime dispatcher + the
// public trigger route and the authed management group with a fake user.
type env struct {
	t       *testing.T
	h       *Handler
	mux     *http.ServeMux
	user    identity.User
	secret  string
	webhook Webhook
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, db := newStore(t, nil)
	uid := seedUser(t, db)

	rt := session.NewRuntime(session.NewMemStore()).WithBus(session.NewMemBus())
	rg := session.NewRunRegistry(rt, rt.Bus())
	d := NewDispatcher(s, rt, rg, nil, nil,
		func(ctx context.Context, userID, teamID, system, model string) (*agent.Loop, error) {
			return agent.New(stubProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100}), nil
		}, "base", nil)

	h := NewHandler(s, d)
	e := &env{t: t, h: h, mux: http.NewServeMux(), user: identity.User{ID: uid, Email: "inb@test.dev"}}
	h.RegisterPublic(e.mux)

	// Authed group with a fake auth middleware: keep a caller set on the
	// request context by the test, defaulting to the env's owner.
	authed := httpx.NewRouter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := e.user
			if cu, ok := identity.UserFromContext(r.Context()); ok {
				u = cu
			}
			next.ServeHTTP(w, r.WithContext(identity.NewContextWithUser(r.Context(), u)))
		})
	})
	h.RegisterAuthed(authed)
	authed.Mount(e.mux, "/api/")

	// Create a webhook through the management API so the env holds its secret.
	rec := e.do(e.user, "POST", "/api/me/inbound", map[string]any{"name": "erp", "notify_url": "https://erp.example/hook"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed webhook: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Webhook webhookDTO `json:"inbound_webhook"`
		Secret  string     `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("seed webhook decode: %v", err)
	}
	e.webhook = Webhook{ID: body.Webhook.ID, Enabled: true}
	e.secret = body.Secret
	return e
}

// do runs an authed request (fake user on the context) against the mux.
func (e *env) do(u identity.User, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if method == "GET" || method == "DELETE" {
		req.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	// Route through the group with the user on the context (GET/DELETE paths
	// are registered on the authed group; the trigger path is public).
	ctx := identity.NewContextWithUser(req.Context(), u)
	req = req.WithContext(ctx)
	e.mux.ServeHTTP(rec, req)
	return rec
}

// trigger posts a signed request to the public trigger endpoint.
func (e *env) trigger(payload string, ts int64, secret string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest("POST", "/api/inbound/"+e.webhook.ID, strings.NewReader(payload))
	req.Header.Set("X-Nowhere-Timestamp", fmt.Sprintf("%d", ts))
	req.Header.Set("X-Nowhere-Signature", "sha256="+e.sign(payload, ts, secret))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) sign(payload string, ts int64, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(fmt.Sprintf("%d.%s", ts, payload)))
	return hex.EncodeToString(m.Sum(nil))
}

func TestTriggerHappyPath(t *testing.T) {
	e := newEnv(t)
	rec := e.trigger(`{"prompt":"hello","metadata":{"ticket":"1"}}`, time.Now().Unix(), e.secret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("trigger: %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "started" || out["run_id"] == "" || out["session_id"] == "" {
		t.Fatalf("trigger payload: %v", out)
	}
}

func TestTriggerRejectsBadSignature(t *testing.T) {
	e := newEnv(t)
	now := time.Now().Unix()
	cases := []struct {
		name   string
		mutate func(req *http.Request)
	}{
		{"wrong secret", func(req *http.Request) { req.Header.Set("X-Nowhere-Signature", "sha256="+e.sign(`{"prompt":"x"}`, now, "wh_wrong")) }},
		{"missing signature", func(req *http.Request) { req.Header.Del("X-Nowhere-Signature") }},
		{"missing timestamp", func(req *http.Request) { req.Header.Del("X-Nowhere-Timestamp") }},
		{"tampered body", func(req *http.Request) {
			// Signed over a DIFFERENT body than the one sent: must fail.
			req.Header.Set("X-Nowhere-Signature", "sha256="+e.sign(`{"prompt":"y"}`, now, e.secret))
		}},
		{"expired timestamp", func(req *http.Request) {
			req.Header.Set("X-Nowhere-Timestamp", fmt.Sprintf("%d", now-6*60))
		}},
		{"future timestamp", func(req *http.Request) {
			req.Header.Set("X-Nowhere-Timestamp", fmt.Sprintf("%d", now+6*60))
		}},
		{"garbage timestamp", func(req *http.Request) { req.Header.Set("X-Nowhere-Timestamp", "abc") }},
		{"unknown webhook", func(req *http.Request) {
			req.URL.Path = "/api/inbound/00000000-0000-0000-0000-000000000000"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := `{"prompt":"x"}`
			req := httptest.NewRequest("POST", "/api/inbound/"+e.webhook.ID, strings.NewReader(payload))
			req.Header.Set("X-Nowhere-Timestamp", fmt.Sprintf("%d", now))
			req.Header.Set("X-Nowhere-Signature", "sha256="+e.sign(payload, now, e.secret))
			c.mutate(req)
			rec := httptest.NewRecorder()
			e.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401", c.name, rec.Code)
			}
		})
	}
}

func TestTriggerRejectsDisabledWebhook(t *testing.T) {
	e := newEnv(t)
	rec := e.do(e.user, "PATCH", "/api/me/inbound/"+e.webhook.ID, map[string]any{"enabled": false})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	rec = e.trigger(`{"prompt":"x"}`, time.Now().Unix(), e.secret)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("trigger disabled: %d, want 401", rec.Code)
	}
}

func TestTriggerRejectsEmptyPrompt(t *testing.T) {
	e := newEnv(t)
	rec := e.trigger(`{"prompt":"  "}`, time.Now().Unix(), e.secret)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt: %d, want 400", rec.Code)
	}
}

func TestTriggerRejectsOversizedPayload(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("a", maxBodyBytes+10)
	rec := e.trigger(`{"prompt":"`+big+`"}`, time.Now().Unix(), e.secret)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d, want 413", rec.Code)
	}
}

func TestManagementCRUD(t *testing.T) {
	e := newEnv(t)
	other := identity.User{ID: "00000000-0000-0000-0000-000000000000", Email: "other@test.dev"}

	// List: the seeded webhook is visible, secret never serialized.
	rec := e.do(e.user, "GET", "/api/me/inbound", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var list struct {
		Webhooks []webhookDTO `json:"inbound_webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Webhooks) != 1 {
		t.Fatalf("list len = %d, want 1", len(list.Webhooks))
	}
	if list.Webhooks[0].ID != e.webhook.ID || list.Webhooks[0].Name != "erp" {
		t.Fatalf("list payload: %+v", list.Webhooks[0])
	}

	// Rotation returns a new secret and the old one stops working.
	rec = e.do(e.user, "POST", "/api/me/inbound/"+e.webhook.ID+"/rotate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	var rot struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rot); err != nil {
		t.Fatal(err)
	}
	if rot.Secret == "" || rot.Secret == e.secret {
		t.Fatalf("rotate did not issue a fresh secret")
	}
	if got := e.trigger(`{"prompt":"x"}`, time.Now().Unix(), e.secret); got.Code != http.StatusUnauthorized {
		t.Fatalf("old secret still works: %d", got.Code)
	}
	if got := e.trigger(`{"prompt":"x"}`, time.Now().Unix(), rot.Secret); got.Code != http.StatusAccepted {
		t.Fatalf("new secret rejected: %d", got.Code)
	}

	// Another user cannot touch it.
	rec = e.do(other, "DELETE", "/api/me/inbound/"+e.webhook.ID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete as other: %d, want 404", rec.Code)
	}
	rec = e.do(other, "POST", "/api/me/inbound/"+e.webhook.ID+"/rotate", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rotate as other: %d, want 404", rec.Code)
	}

	// Owner deletes it; list is then empty.
	rec = e.do(e.user, "DELETE", "/api/me/inbound/"+e.webhook.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(e.user, "GET", "/api/me/inbound", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Webhooks) != 0 {
		t.Fatalf("list after delete: %d entries", len(list.Webhooks))
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	for _, body := range []map[string]any{
		{},
		{"name": ""},
		{"name": "x", "agent_def": "a", "system_prompt": "b"},
		{"name": "x", "notify_url": "ftp://bad"},
	} {
		rec := e.do(e.user, "POST", "/api/me/inbound", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %v: status %d, want 400", body, rec.Code)
		}
	}
}
