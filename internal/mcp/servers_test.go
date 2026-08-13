package mcp

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestParseServers(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
		want int
	}{
		{"empty", "", true, 0},
		{"one server", `[{"name":"searxng","url":"https://x.example.com/mcp"}]`, true, 1},
		{"headers and timeout", `[{"name":"kb","url":"https://kb.example.com/mcp","headers":{"Authorization":"Bearer t"},"timeout":"45s"}]`, true, 1},
		{"two servers", `[{"name":"a","url":"https://a.example.com/mcp"},{"name":"b","url":"https://b.example.com/mcp"}]`, true, 2},
		{"bad json", `[{"name":`, false, 0},
		{"missing name", `[{"url":"https://x.example.com/mcp"}]`, false, 0},
		{"duplicate name", `[{"name":"a","url":"https://a.example.com/mcp"},{"name":"a","url":"https://b.example.com/mcp"}]`, false, 0},
		{"non-http url", `[{"name":"a","url":"file:///etc/x"}]`, false, 0},
		{"bare host", `[{"name":"a","url":"kb.example.com/mcp"}]`, false, 0},
		{"bad timeout", `[{"name":"a","url":"https://a.example.com/mcp","timeout":"later"}]`, false, 0},
	}
	for _, c := range cases {
		got, err := ParseServers(c.raw)
		if c.ok && err != nil {
			t.Errorf("%s: expected ok, got %v", c.name, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
			continue
		}
		if c.ok && len(got) != c.want {
			t.Errorf("%s: got %d servers, want %d", c.name, len(got), c.want)
		}
	}
}

func TestTimeoutFor(t *testing.T) {
	c := ServerConfig{Timeout: "45s"}
	if d := c.timeoutFor(); d != 45*time.Second {
		t.Errorf("timeoutFor = %v, want 45s", d)
	}
	if d := (ServerConfig{}).timeoutFor(); d != 0 {
		t.Errorf("empty timeoutFor = %v, want 0 (client default)", d)
	}
}

func TestNewManagerFromJSON(t *testing.T) {
	m, err := NewManagerFromJSON("")
	if err != nil || m != nil {
		t.Errorf("empty config: manager=%v err=%v, want nil manager", m, err)
	}
	m, err = NewManagerFromJSON(`[{"name":"a","url":"https://a.example.com/mcp"},{"name":"b","url":"https://b.example.com/mcp"}]`)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if got := m.ServerNames(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("ServerNames = %v, want [a b]", got)
	}
	if _, err := NewManagerFromJSON(`[{"name":"a","url":"nope"}]`); err == nil {
		t.Error("bad url should error")
	}
}

// TestEmptyManagerAppliesRuntimeConfig proves the cold-start contract: a
// manager built without boot servers can still reconcile a runtime
// mcp_servers value (the admin console enables MCP without a restart).
func TestEmptyManagerAppliesRuntimeConfig(t *testing.T) {
	m := NewEmptyManager()
	if got := m.ServerNames(); len(got) != 0 {
		t.Fatalf("empty manager servers = %v, want none", got)
	}
	if got := len(m.Tools()); got != 0 {
		t.Errorf("empty manager tools = %d, want 0", got)
	}
	added, removed, err := m.Apply(`[{"name":"searxng","url":"https://searxng.example.com/mcp"}]`)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(removed) != 0 || len(added) != 1 || added[0].Server() != "searxng" {
		t.Fatalf("added=%v removed=%v, want [searxng] added", serverNames(added), removed)
	}
	if got := m.ServerNames(); len(got) != 1 || got[0] != "searxng" {
		t.Errorf("servers after apply = %v, want [searxng]", got)
	}
}

func TestManagerToolsBeforeConnectEmpty(t *testing.T) {
	m, err := NewManagerFromJSON(`[{"name":"a","url":"https://a.example.com/mcp"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(m.Tools()); got != 0 {
		t.Errorf("tools before connect = %d, want 0", got)
	}
}

func TestManagerAggregatesClients(t *testing.T) {
	m, err := NewManagerFromJSON(`[{"name":"a","url":"https://a.example.com/mcp"},{"name":"b","url":"https://b.example.com/mcp"}]`)
	if err != nil {
		t.Fatal(err)
	}
	clients := m.Clients()
	if len(clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(clients))
	}
	if clients[0].Server() != "a" || clients[1].Server() != "b" {
		t.Errorf("client servers = %q, %q", clients[0].Server(), clients[1].Server())
	}
}

// TestManagerApplyReconciles proves the runtime reconfigure contract: added
// servers are returned for a reconnect loop, removed ones are named for
// cancellation, and unchanged servers keep their exact client instance (live
// session survives an unrelated edit).
func TestManagerApplyReconciles(t *testing.T) {
	m, err := NewManagerFromJSON(`[{"name":"a","url":"https://a.example.com/mcp"},{"name":"b","url":"https://b.example.com/mcp"}]`)
	if err != nil {
		t.Fatal(err)
	}
	origA := m.Clients()[0]
	origB := m.Clients()[1]

	// Retune: keep a as-is, change b's endpoint, add c, drop nothing else.
	added, removed, err := m.Apply(`[
		{"name":"a","url":"https://a.example.com/mcp"},
		{"name":"b","url":"https://b.example.com/mcp/v2"},
		{"name":"c","url":"https://c.example.com/mcp"}
	]`)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	if len(added) != 2 || added[0].Server() != "b" || added[1].Server() != "c" {
		t.Fatalf("added = %v, want [b c]", serverNames(added))
	}
	clients := m.Clients()
	if len(clients) != 3 {
		t.Fatalf("clients = %d, want 3", len(clients))
	}
	if clients[0] != origA {
		t.Error("unchanged server a did not keep its client instance")
	}
	_ = origB // b was rebuilt; origB is dropped

	// Drop a and c; a's client instance is removed and named.
	added, removed, err = m.Apply(`[{"name":"b","url":"https://b.example.com/mcp/v2"}]`)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("added after drop = %v, want none", serverNames(added))
	}
	if len(removed) != 2 || removed[0] != "a" || removed[1] != "c" {
		t.Fatalf("removed = %v, want [a c]", removed)
	}
	if got := m.ServerNames(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("servers after drop = %v, want [b]", got)
	}

	// Empty config disables MCP entirely.
	added, removed, err = m.Apply("")
	if err != nil {
		t.Fatalf("apply empty: %v", err)
	}
	if len(added) != 0 || len(removed) != 1 || removed[0] != "b" {
		t.Fatalf("apply empty: added=%v removed=%v, want none added and [b] removed", serverNames(added), removed)
	}
	if got := m.ServerNames(); len(got) != 0 {
		t.Fatalf("servers after empty = %v, want none", got)
	}

	// A malformed list is an error and leaves the set untouched.
	if _, _, err := m.Apply(`[{"name":"x","url":"nope"}]`); err == nil {
		t.Error("malformed apply should error")
	}
	if got := m.ServerNames(); len(got) != 0 {
		t.Fatalf("malformed apply mutated clients: %v", got)
	}
}

func serverNames(clients []*Client) []string {
	var out []string
	for _, c := range clients {
		out = append(out, c.Server())
	}
	return out
}

// TestApplyClosesDroppedAndRebuiltClients proves that Apply releases the
// session of every client it discards (removed or config-changed), while an
// unchanged client keeps its live session.
func TestApplyClosesDroppedAndRebuiltClients(t *testing.T) {
	hs := newTestServer(t)
	url := hs.URL
	m, err := NewManagerFromJSON(`[{"name":"a","url":"` + url + `"},{"name":"b","url":"` + url + `"}]`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	for _, c := range m.Clients() {
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("connect %s: %v", c.Server(), err)
		}
	}
	origA := m.Clients()[0]
	origB := m.Clients()[1]

	// Retune: keep a as-is, change b's endpoint (rebuilt), add c.
	if _, _, err := m.Apply(`[{"name":"a","url":"` + url + `"},{"name":"b","url":"` + url + `/v2"},{"name":"c","url":"https://c.example.com/mcp"}]`); err != nil {
		t.Fatalf("apply: %v", err)
	}
	origA.mu.Lock()
	if origA.session == nil {
		t.Error("unchanged client a must keep its live session")
	}
	origA.mu.Unlock()
	origB.mu.Lock()
	if origB.session != nil {
		t.Error("rebuilt client b must have its session closed")
	}
	origB.mu.Unlock()

	// Drop a: its session must be closed too.
	if _, _, err := m.Apply(`[{"name":"c","url":"https://c.example.com/mcp"}]`); err != nil {
		t.Fatalf("apply drop: %v", err)
	}
	origA.mu.Lock()
	defer origA.mu.Unlock()
	if origA.session != nil {
		t.Error("dropped client a must have its session closed")
	}
}

func TestHeaderTransportInjectsHeaders(t *testing.T) {
	var got http.Header
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Clone()
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	ts := headerTransport{base: base, headers: map[string]string{"Authorization": "Bearer sekrit"}}
	req, err := http.NewRequest(http.MethodGet, "https://a.example.com/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want Bearer sekrit", got.Get("Authorization"))
	}
}

type roundTripFunc func(r *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
