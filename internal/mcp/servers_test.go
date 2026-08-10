package mcp

import (
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
