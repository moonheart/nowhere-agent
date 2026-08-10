package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	"nowhere-agent/internal/toolruntime"
)

// ServerConfig is one MCP server entry from the MCP_SERVERS JSON config:
//
//	[{"name": "searxng", "url": "https://searxng-mcp.example.com/mcp",
//	  "headers": {"Authorization": "Bearer x"}, "timeout": "30s"}]
//
// name prefixes every adapted tool (mcp_<name>_<tool>); headers are sent on
// every request (bearer tokens, api keys for enterprise MCP servers); timeout
// bounds one tool call (empty = the client default).
type ServerConfig struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// ParseServers decodes and validates the MCP_SERVERS JSON. Duplicate or empty
// names and non-http(s) URLs are rejected — a misconfigured server list should
// fail at boot, not silently drop tools a deployment believes it has.
func ParseServers(raw string) ([]ServerConfig, error) {
	if raw == "" {
		return nil, nil
	}
	var cfgs []ServerConfig
	if err := json.Unmarshal([]byte(raw), &cfgs); err != nil {
		return nil, fmt.Errorf("MCP_SERVERS: %w", err)
	}
	if len(cfgs) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for i, c := range cfgs {
		if c.Name == "" {
			return nil, fmt.Errorf("MCP_SERVERS[%d]: name is required", i)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("MCP_SERVERS: duplicate server name %q", c.Name)
		}
		seen[c.Name] = true
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("MCP_SERVERS[%d] %q: url must be an absolute http(s) URL, got %q", i, c.Name, c.URL)
		}
		if c.Timeout != "" {
			if _, err := time.ParseDuration(c.Timeout); err != nil {
				return nil, fmt.Errorf("MCP_SERVERS[%d] %q: bad timeout %q: %v", i, c.Name, c.Timeout, err)
			}
		}
	}
	return cfgs, nil
}

// timeoutFor parses a config timeout into a duration, zero when unset.
func (c ServerConfig) timeoutFor() time.Duration {
	if c.Timeout == "" {
		return 0
	}
	d, _ := time.ParseDuration(c.Timeout)
	return d
}

// Manager owns the configured MCP clients. It is the server's handle for the
// whole MCP surface: connect/reconnect each client in the background and read
// the aggregated tool set for per-run registration. Safe for concurrent use
// after construction.
type Manager struct {
	clients []*Client
}

// NewManager builds a Manager over the given server configs.
func NewManager(cfgs []ServerConfig) (*Manager, error) {
	if len(cfgs) == 0 {
		return nil, nil
	}
	m := &Manager{}
	for _, c := range cfgs {
		m.clients = append(m.clients, NewWithHeaders(c.Name, c.URL, c.timeoutFor(), c.Headers))
	}
	return m, nil
}

// NewManagerFromJSON parses the MCP_SERVERS JSON and builds the Manager in one
// step. An empty string yields a nil manager (MCP disabled). A malformed list
// is an error — fail at boot rather than silently drop servers.
func NewManagerFromJSON(raw string) (*Manager, error) {
	cfgs, err := ParseServers(raw)
	if err != nil {
		return nil, err
	}
	return NewManager(cfgs)
}

// Clients returns the managed clients (for the background reconnect loop).
func (m *Manager) Clients() []*Client { return m.clients }

// Tools aggregates every adapted tool across all connected servers. Tools of a
// server that has not connected yet are absent; the reconnect loop re-populates
// them on a successful handshake.
func (m *Manager) Tools() []toolruntime.Tool {
	var out []toolruntime.Tool
	for _, c := range m.clients {
		out = append(out, c.Tools()...)
	}
	return out
}

// ServerNames returns the configured server names, sorted, for logging.
func (m *Manager) ServerNames() []string {
	names := make([]string, 0, len(m.clients))
	for _, c := range m.clients {
		names = append(names, c.Server())
	}
	sort.Strings(names)
	return names
}
