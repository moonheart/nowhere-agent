package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/routing"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/usage"
)

// Manual consolidation: the HTTP contract. The single-flight semantics
// themselves are pinned in internal/dreaming; what matters here is that the
// route is self-scoped, that a busy runner is a 409 rather than an error, and
// that an unwired runner degrades instead of panicking.

// recordingSource captures which account a pass was scoped to, which is the one
// thing the handler is responsible for getting right.
type recordingSource struct {
	askedFor chan string
	gate     chan struct{} // non-nil: hold the pass open until closed
}

func (s *recordingSource) PendingSessions(context.Context) ([]dreaming.PendingSession, error) {
	return nil, nil
}

func (s *recordingSource) PendingSessionsForUser(_ context.Context, userID string) ([]dreaming.PendingSession, error) {
	select {
	case s.askedFor <- userID:
	default:
	}
	if s.gate != nil {
		<-s.gate
	}
	return nil, nil
}

func (s *recordingSource) Episodes(context.Context, string, int64) ([]session.StoredMessage, error) {
	return nil, nil
}
func (s *recordingSource) MarkProcessed(context.Context, string, int64) error { return nil }

// stubLLM is never reached (no pending sessions) but the worker requires one.
type stubLLM struct{}

func (stubLLM) Complete(context.Context, string) (string, int, error) { return "", 0, nil }
func (stubLLM) CompleteJSON(context.Context, string, *provider.JSONResponseSpec, any) (int, error) {
	return 0, nil
}

// decodeInto unmarshals a recorded response into a typed struct. decodeBody's
// map[string]any cannot express "the last field was absent" without type
// gymnastics, and that distinction is exactly what these tests assert.
func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// withDreaming rebuilds the env's routes with a consolidation runner attached.
func (e *env) withDreaming(src dreaming.EpisodeSource) *dreaming.Runner {
	e.t.Helper()
	w := dreaming.NewWorker(src, memory.NewMemPort(), stubLLM{}, dreaming.Budget{MaxTokens: 1000})
	r := dreaming.NewRunner(w, context.Background())
	e.t.Cleanup(r.Wait)

	h := NewHandler(e.svc, routing.NewPGKeyStore(e.db, "platform-key"), usage.NewStore(e.db), e.mem).
		WithDreaming(r)
	e.mux = http.NewServeMux()
	h.RegisterAuthed(e.mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(identity.NewContextWithUser(req.Context(), e.actor)))
		})
	})
	return r
}

// Without a runner the console must still serve: a deployment with no provider
// configured has no worker, and that should cost it this one button rather than
// the whole page.
func TestDreamUnavailableWithoutRunner(t *testing.T) {
	e := newEnv(t)
	u := e.user(identity.PlatformRoleUser)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/me/dream"},
		{"POST", "/api/me/dream"},
	} {
		if got := e.as(u, tc.method, tc.path, nil).Code; got != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, got)
		}
	}
}

// The route is self-scoped by construction: the handler takes the account from
// the authenticated context, so there is no parameter through which one user
// could aim a pass at another's sessions.
func TestDreamTriggerIsScopedToCaller(t *testing.T) {
	e := newEnv(t)
	src := &recordingSource{askedFor: make(chan string, 4)}
	r := e.withDreaming(src)

	u := e.user(identity.PlatformRoleUser)
	other := e.user(identity.PlatformRoleUser)

	if got := e.as(u, "POST", "/api/me/dream", nil).Code; got != http.StatusAccepted {
		t.Fatalf("POST /api/me/dream = %d, want 202", got)
	}
	r.Wait()

	select {
	case id := <-src.askedFor:
		if id != u.ID {
			t.Errorf("pass scoped to %q, want the caller %q", id, u.ID)
		}
	default:
		t.Fatal("the pass never asked for any account's sessions")
	}

	// The other account sees no history from it.
	var body struct {
		Running bool `json:"running"`
		Last    *struct {
			Tokens int `json:"tokens"`
		} `json:"last"`
	}
	decodeInto(t, e.as(other, "GET", "/api/me/dream", nil), &body)
	if body.Last != nil {
		t.Error("one account's pass showed up in another's status")
	}
}

func TestDreamStatusReportsCompletedPass(t *testing.T) {
	e := newEnv(t)
	r := e.withDreaming(&recordingSource{askedFor: make(chan string, 4)})
	u := e.user(identity.PlatformRoleUser)

	e.as(u, "POST", "/api/me/dream", nil)
	r.Wait()

	var body struct {
		Running bool `json:"running"`
		Mine    bool `json:"mine"`
		Last    *struct {
			StartedAt string `json:"started_at"`
			Episodes  int    `json:"episodes"`
			Error     string `json:"error"`
		} `json:"last"`
	}
	rec := e.as(u, "GET", "/api/me/dream", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me/dream = %d, want 200", rec.Code)
	}
	decodeInto(t, rec, &body)
	if body.Running {
		t.Error("no pass should be in flight")
	}
	if body.Last == nil {
		t.Fatal("the completed pass should be reported")
	}
	if body.Last.Error != "" {
		t.Errorf("error = %q, want none", body.Last.Error)
	}
	if body.Last.StartedAt == "" {
		t.Error("the pass should carry a start time")
	}
}

// 409 rather than 429: this is a single-flight conflict the caller resolves by
// waiting for the running pass, not a rate limit they resolve by backing off.
func TestDreamTriggerWhileRunningIsConflict(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	src := &recordingSource{askedFor: make(chan string, 4), gate: gate}
	r := e.withDreaming(src)
	u := e.user(identity.PlatformRoleUser)

	if got := e.as(u, "POST", "/api/me/dream", nil).Code; got != http.StatusAccepted {
		t.Fatalf("first trigger = %d, want 202", got)
	}
	<-src.askedFor // the pass is in flight and held at the gate

	if got := e.as(u, "POST", "/api/me/dream", nil).Code; got != http.StatusConflict {
		t.Errorf("second trigger = %d, want 409", got)
	}

	var body struct {
		Running bool `json:"running"`
		Mine    bool `json:"mine"`
	}
	decodeInto(t, e.as(u, "GET", "/api/me/dream", nil), &body)
	if !body.Running || !body.Mine {
		t.Errorf("status = %+v, want running and mine", body)
	}

	close(gate)
	r.Wait()
}
