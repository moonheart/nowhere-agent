package builtin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nowhere-agent/internal/toolruntime"
)

// HTTPToolName is the built-in tool that calls an external HTTP API from the
// allowlist (enterprise integration): the agent can query internal services
// (ERP, CRM, knowledge APIs) that the operator has explicitly allowed.
const HTTPToolName = "http_request"

// httpMaxBody caps the response body a tool call returns to the model.
const httpMaxBody = 256 << 10 // 256 KiB

// httpDefaultTimeout bounds one request when the model sets none.
const httpDefaultTimeout = 30 * time.Second

// httpToolArgs is the tool's input schema as a literal (kept in sync with the
// args parsing in Call).
var httpToolArgs = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"method": map[string]any{"type": "string", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}, "description": "HTTP method (default GET)"},
		"url":    map[string]any{"type": "string", "description": "Absolute http(s) URL whose host is on the configured allowlist"},
		"headers": map[string]any{
			"type":        "object",
			"additionalProperties": map[string]any{"type": "string"},
			"description": "Optional request headers (never Authorization unless you own the target)",
		},
		"body":    map[string]any{"type": "string", "description": "Request body for POST/PUT/PATCH (sent as-is)"},
		"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds (default 30, max 60)"},
	},
	"required": []string{"url"},
	"additionalProperties": false,
}

// AllowlistFunc decides whether a URL may be called by http_request. A nil
// AllowlistFunc (no allowlist configured) disables the tool at registration.
type AllowlistFunc func(rawURL string) bool

// httpRequestTool calls external HTTP APIs, confined to the configured host
// allowlist. RiskNetwork so the permission gate controls it like MCP tools.
type httpRequestTool struct {
	allow   AllowlistFunc
	client  *http.Client
	timeout time.Duration
}

// NewHTTPRequest returns the http_request tool, gated on allow. allow decides
// whether a requested URL is permitted; nil disables the tool.
func NewHTTPRequest(allow AllowlistFunc, timeout time.Duration) toolruntime.Tool {
	if allow == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = httpDefaultTimeout
	}
	return &httpRequestTool{
		allow:  allow,
		client: &http.Client{},
		// The tool-level timeout bounds one call; the per-request timeout is
		// capped below by the caller's argument.
		timeout: timeout,
	}
}

func (t *httpRequestTool) Name() string { return HTTPToolName }
func (t *httpRequestTool) Description() string {
	return "Call an external HTTP API whose host is on the configured allowlist (enterprise " +
		"integration: internal ERP/CRM/knowledge services). Returns the response status, headers, and body."
}
func (t *httpRequestTool) Schema() map[string]any { return httpToolArgs }
func (t *httpRequestTool) Risk() toolruntime.Risk { return toolruntime.RiskNetwork }
func (t *httpRequestTool) Timeout() time.Duration { return t.timeout }

func (t *httpRequestTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	rawURL, _ := args["url"].(string)
	if rawURL == "" {
		return toolruntime.Result{Content: "http_request: url is required", IsError: true}, nil
	}
	if !t.allow(rawURL) {
		return toolruntime.Result{Content: "http_request: host is not on the allowlist", IsError: true}, nil
	}
	method, _ := args["method"].(string)
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)
	valid := map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
		http.MethodPatch: true, http.MethodDelete: true, http.MethodHead: true}
	if !valid[method] {
		return toolruntime.Result{Content: fmt.Sprintf("http_request: unsupported method %q", method), IsError: true}, nil
	}

	var body io.Reader
	if b, ok := args["body"].(string); ok && b != "" {
		body = strings.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("http_request: %v", err), IsError: true}, nil
	}
	if hs, ok := args["headers"].(map[string]any); ok {
		for k, v := range hs {
			if s, ok := v.(string); ok && s != "" {
				req.Header.Set(k, s)
			}
		}
	}

	timeout := t.timeout
	if sec, ok := args["timeout"].(float64); ok && sec > 0 {
		if d := time.Duration(sec) * time.Second; d < timeout {
			timeout = d
		}
	}
	client := t.client
	if timeout != t.timeout {
		client = &http.Client{Timeout: timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("http_request: %v", err), IsError: true}, nil
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, httpMaxBody+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("http_request: read body: %v", err), IsError: true}, nil
	}
	if len(raw) > httpMaxBody {
		return toolruntime.Result{Content: fmt.Sprintf("http_request: response exceeds %d bytes; body truncated", httpMaxBody), IsError: true}, nil
	}

	// Trim the body for the model: it is capped, but a 256KiB JSON blob is
	// still too much context — keep the first 16 KiB with a marker.
	const modelLimit = 16 << 10
	bodyText := strings.TrimSpace(string(raw))
	if len(bodyText) > modelLimit {
		bodyText = bodyText[:modelLimit] + "\n…(truncated)"
	}
	return toolruntime.Result{Content: fmt.Sprintf("status: %s\n%s", resp.Status, bodyText)}, nil
}

// Allowlist compiles host patterns into a matcher. Patterns:
//
//	api.example.com        exact host (any port); api.example.com:8443 pins the port
//	*.example.com          example.com and any subdomain
//	10.0.0.0/8             CIDR (IP allowlisting for internal networks)
//	*                      any host (explicit full open)
//
// Everything is matched case-insensitively on the host.
func Allowlist(patterns []string) (AllowlistFunc, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	type rule struct {
		host   string // exact host (lowercased), "" when CIDR
		prefix string // "*.example.com" → ".example.com"
		port   string // exact port or "" (any)
		ipNet  *net.IPNet
	}
	rules := make([]rule, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		host, port := splitHostPort(p)
		host = strings.ToLower(host)
		if _, ipnet, err := net.ParseCIDR(host); err == nil {
			rules = append(rules, rule{ipNet: ipnet})
			continue
		}
		switch {
		case host == "*":
			rules = append(rules, rule{host: "*"})
		case strings.HasPrefix(host, "*."):
			rules = append(rules, rule{prefix: host[1:]}) // ".example.com"
		default:
			rules = append(rules, rule{host: host, port: port})
		}
	}
	return func(rawURL string) bool {
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return false
		}
		h, p := splitHostPort(u.Host)
		h = strings.ToLower(h)
		if ip := net.ParseIP(h); ip != nil {
			for _, r := range rules {
				if r.ipNet != nil && r.ipNet.Contains(ip) {
					return true
				}
			}
			// Exact-host rules also match IP literals.
		}
		for _, r := range rules {
			switch {
			case r.host == "*":
				return true
			case r.ipNet != nil:
				continue
			case r.host != "":
				if r.host != h {
					continue
				}
				if r.port != "" && r.port != p {
					continue
				}
				return true
			case r.prefix != "":
				if !strings.HasSuffix(h, r.prefix) || h == r.prefix[1:] {
					continue
				}
				if r.port != "" && r.port != p {
					continue
				}
				return true
			}
		}
		return false
	}, nil
}

// splitHostPort splits "host" or "host:port"; a bare "host" keeps port empty.
func splitHostPort(hostport string) (host, port string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, ""
	}
	return host, port
}
