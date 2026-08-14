package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nowhere-agent/internal/netutil"
	"nowhere-agent/internal/toolruntime"
	"nowhere-agent/internal/webhook"
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
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
			"description":          "Optional request headers (never Authorization unless you own the target)",
		},
		"body":    map[string]any{"type": "string", "description": "Request body for POST/PUT/PATCH (sent as-is)"},
		"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds; only ever shortens the call — the effective ceiling is the configured tool timeout (HTTP_TOOL_TIMEOUT, default 30s), a larger value is clamped, it cannot extend the call"},
	},
	"required":             []string{"url"},
	"additionalProperties": false,
}

// AllowlistFunc decides whether a URL may be called by http_request. A nil
// AllowlistFunc (no allowlist configured) disables the tool at registration.
type AllowlistFunc func(rawURL string) bool

// httpRequestTool calls external HTTP APIs, confined to the configured host
// allowlist. RiskNetwork so the permission gate controls it like MCP tools.
// The string allowlist decides WHICH hosts the operator permits; the SSRF
// guard (webhook.Guard) then resolves every hostname target and refuses any
// address that is private/loopback/etc. unless it falls in an explicitly
// allowlisted CIDR (or IP literal) — closing the split-horizon-DNS hole where
// an allowlisted name resolves to an internal address — and pins the vetted
// addresses into the request so the transport dials exactly them.
type httpRequestTool struct {
	allow    AllowlistFunc
	guard    *webhook.Guard
	resolver webhook.Resolver
	client   *http.Client
	timeout  time.Duration
}

// NewHTTPRequest returns the http_request tool, gated on the allowlist
// patterns. An empty pattern list disables the tool (nil return): no
// allowlist, no tool. The patterns keep their documented string semantics
// (exact host, *.subdomain, CIDR, *); on top of that, every hostname target is
// resolved at call time and refused when any resolved address is
// private/loopback/link-local unless it falls in an explicitly allowlisted
// CIDR or IP literal — the same resolve→vet→pin pipeline as webhook delivery
// (internal/webhook ssrf guard). The tool-level timeout bounds one call; the
// per-request timeout is capped below by the caller's argument.
func NewHTTPRequest(patterns []string, timeout time.Duration) toolruntime.Tool {
	allow, err := Allowlist(patterns)
	if err != nil || allow == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = httpDefaultTimeout
	}
	t := &httpRequestTool{
		allow:    allow,
		resolver: net.DefaultResolver,
		timeout:  timeout,
	}
	t.guard, err = webhook.NewGuardWithResolver(explicitIPAllowances(patterns), nil, t.resolver)
	if err != nil {
		return nil
	}
	// The default redirect policy only re-checks nothing: without a custom
	// CheckRedirect an allowlisted host could 302 the request to a private
	// target that the allowlist gate never sees. Re-verify every hop — both
	// the string rules and the SSRF vetting + pin.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = t.dialContext
	t.client = &http.Client{Transport: tr, CheckRedirect: t.checkRedirect}
	return t
}

// explicitIPAllowances collects the allowlist's explicit IP allowances — CIDR
// patterns and bare IP-literal patterns (as /32|/128) — which the SSRF guard
// treats as the operator's escape hatch for legitimately internal targets
// (mirroring webhook's allowlist CIDRs). A hostname rule grants no IP
// exemption: its addresses are always vetted.
func explicitIPAllowances(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		host, _ := splitHostPort(strings.TrimSpace(p))
		if _, _, err := net.ParseCIDR(host); err == nil {
			out = append(out, host)
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.To4() != nil {
				out = append(out, host+"/32")
			} else {
				out = append(out, host+"/128")
			}
		}
	}
	return out
}

// checkRedirect re-verifies every redirect hop against the allowlist AND the
// SSRF guard, so an allowlisted host cannot smuggle the request elsewhere via
// a 302 — neither past the host rules nor past the private-address vetting
// (each hop is vetted and re-pinned into the hop request).
func (t *httpRequestTool) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if !t.allow(req.URL.String()) {
		return errors.New("redirect target not allowed")
	}
	return t.guard.PinRequest(req, req.URL.String())
}

// dialContext closes the check-to-dial race: when the request context carries
// the SSRF vetting pin, the transport dials exactly those vetted addresses —
// never re-resolving the host — so a host that rebinds between the vetting
// and the connection cannot redirect the dial to a private address. Shares
// the webhook guard's dial hook.
func (t *httpRequestTool) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.guard.DialContext(ctx, network, addr)
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
	// The model's timeout only ever SHORTENS the call: values above the
	// configured tool timeout (HTTP_TOOL_TIMEOUT, the schema's documented
	// ceiling) are clamped down, never honored as an extension.
	if sec, ok := args["timeout"].(float64); ok && sec > 0 {
		if d := time.Duration(sec) * time.Second; d < timeout {
			timeout = d
		}
	}
	client := t.client
	if timeout != t.timeout {
		client = &http.Client{Transport: t.client.Transport, CheckRedirect: t.checkRedirect, Timeout: timeout}
	}

	// SSRF vetting runs here, on the caller's URL: hostname targets are
	// resolved and refused when any address is private/loopback/etc. unless
	// explicitly CIDR-allowed, and the vetted addresses are pinned into the
	// request so the transport dials exactly them (a host that rebinds
	// between this check and the connection cannot redirect the dial).
	if err := t.guard.PinRequest(req, rawURL); err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("http_request: %v", err), IsError: true}, nil
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
		// url.Parse keeps the brackets on a portless IPv6 literal
		// ("[::1]" instead of "::1"), so strip them or the literal can
		// never be vetted as an IP (mirrors webhook's hostnameOnly).
		h = strings.Trim(h, "[]")
		if ip := net.ParseIP(h); ip != nil {
			for _, r := range rules {
				if r.ipNet != nil && r.ipNet.Contains(ip) {
					return true
				}
			}
			// Translation schemes (NAT64/6to4/ISATAP/4-in-6) reach an
			// embedded IPv4 on the caller's behalf: match the CIDR rules
			// against that too, or a [64:ff9b::10.0.0.1] target misses a
			// 10.0.0.0/8 rule in an IPv6-only (NAT64) setup. Every reading
			// of an ambiguous NAT64 address is tried (see
			// netutil.EmbeddedIPv4s).
			if v4s := netutil.EmbeddedIPv4s(ip); len(v4s) > 0 {
				for _, v4 := range v4s {
					for _, r := range rules {
						if r.ipNet != nil && r.ipNet.Contains(v4) {
							return true
						}
					}
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
