package builtin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/toolruntime"
	"nowhere-agent/internal/webhook"
)

// fakeResolver returns a fixed address set per host, no real DNS — the
// resolver seam the SSRF guard exposes (webhook.Resolver).
type fakeResolver struct {
	addrs map[string][]net.IPAddr
	err   error
}

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return f.addrs[host], f.err
}

// toolWithResolver builds the http_request tool with a fake DNS resolver so
// hostname vetting is tested without touching real DNS.
func toolWithResolver(t *testing.T, patterns []string, r webhook.Resolver) toolruntime.Tool {
	t.Helper()
	tool := NewHTTPRequest(patterns, 10*time.Second)
	if tool == nil {
		return nil
	}
	ht := tool.(*httpRequestTool)
	guard, err := webhook.NewGuardWithResolver(explicitIPAllowances(patterns), nil, r)
	if err != nil {
		t.Fatal(err)
	}
	ht.guard = guard
	return tool
}

func TestAllowlistMatches(t *testing.T) {
	allow, err := Allowlist([]string{"api.example.com", "*.corp.cn", "10.0.0.0/8", "svc.example.com:8443"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://api.example.com/v1/orders", true},
		{"http://api.example.com:8080/x", true}, // exact host, any port
		{"https://api.example.com.evil.com/x", false},
		{"https://evilapi.example.com/x", false}, // suffix, not subdomain
		{"https://kb.corp.cn/search", true},      // subdomain of *.corp.cn
		{"https://corp.cn/x", false},             // *.corp.cn does not match apex
		{"https://svc.example.com:8443/x", true},
		{"https://svc.example.com/x", false}, // port pinned
		{"https://10.1.2.3/x", true},         // CIDR
		{"https://10.9.9.9/x", true},
		{"https://11.0.0.1/x", false},
		{"https://[64:ff9b::a00:1]/x", true},       // NAT64 embedding 10.0.0.1, inside 10.0.0.0/8
		{"https://[64:ff9b:1:a00:101::]/x", true},  // NAT64 /48, u≠0, embedding 10.0.1.0, inside 10.0.0.0/8
		{"https://[64:ff9b:1:0:a:0:100::]/x", true}, // NAT64 PL=64: 10.0.0.1 at bytes 9-12, inside 10.0.0.0/8
		{"https://[2001:db8::5efe:a00:1]/x", true}, // ISATAP embedding 10.0.0.1, inside 10.0.0.0/8
		{"https://[64:ff9b::c0a8:101]/x", false},   // NAT64 embedding 192.168.1.1, outside 10.0.0.0/8
		{"file://api.example.com/x", false},
		{"https:///nohost", false},
		{"https://other.com/x", false},
	}
	for _, c := range cases {
		if got := allow(c.url); got != c.want {
			t.Errorf("%s: got %v, want %v", c.url, got, c.want)
		}
	}
}

func TestAllowlistWildcard(t *testing.T) {
	allow, err := Allowlist([]string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	if !allow("https://anything.example.net/x") {
		t.Error("wildcard should match any host")
	}
	if allow("ftp://anything.example.net/x") {
		t.Error("wildcard must not allow non-http schemes")
	}
}

func TestHTTPRequestCallsAllowedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "1")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tool := NewHTTPRequest([]string{"127.0.0.1"}, 10*time.Second)
	if tool == nil {
		t.Fatal("tool should be registered with an allowlist")
	}
	res, err := tool.Call(context.Background(), map[string]any{"url": srv.URL, "method": "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if !strings.Contains(res.Content, `"ok":true`) {
		t.Errorf("body missing from result: %s", res.Content)
	}
	if !strings.HasPrefix(res.Content, "status: 200") {
		t.Errorf("status prefix missing: %s", res.Content)
	}
}

func TestHTTPRequestRejectsNonAllowlistedHost(t *testing.T) {
	tool := NewHTTPRequest([]string{"api.example.com"}, 10*time.Second)
	res, err := tool.Call(context.Background(), map[string]any{"url": "https://evil.example.net/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "allowlist") {
		t.Errorf("expected allowlist rejection, got %s", res.Content)
	}
}

func TestHTTPRequestRedirectToNonAllowlistedTargetRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
	}))
	defer srv.Close()

	tool := NewHTTPRequest([]string{"127.0.0.1"}, 10*time.Second)
	res, err := tool.Call(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "redirect target not allowed") {
		t.Errorf("expected redirect target rejection, got %s", res.Content)
	}
}

func TestHTTPRequestRedirectToAllowlistedTargetFollowed(t *testing.T) {
	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tool := NewHTTPRequest([]string{"127.0.0.1"}, 10*time.Second)
	res, err := tool.Call(context.Background(), map[string]any{"url": srv.URL + "/start"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if !redirected {
		t.Error("redirect to an allowlisted host was not followed")
	}
}

func TestHTTPRequestNilAllowlistDisablesTool(t *testing.T) {
	if got := NewHTTPRequest(nil, 10*time.Second); got != nil {
		t.Error("nil allowlist should disable the tool")
	}
}

func TestHTTPRequestSendsBodyAndHeaders(t *testing.T) {
	var gotMethod, gotBody, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tool := NewHTTPRequest([]string{"127.0.0.1"}, 10*time.Second)
	res, err := tool.Call(context.Background(), map[string]any{
		"url":     srv.URL,
		"method":  "POST",
		"headers": map[string]any{"Authorization": "Bearer x", "X-Custom": "1"},
		"body":    `{"q":"你好"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if gotMethod != "POST" || gotAuth != "Bearer x" || gotBody != `{"q":"你好"}` {
		t.Errorf("request mismatch: method=%s auth=%s body=%s", gotMethod, gotAuth, gotBody)
	}
}

func TestHTTPRequestInvalidArgs(t *testing.T) {
	tool := NewHTTPRequest([]string{"127.0.0.1"}, 10*time.Second)

	if res, _ := tool.Call(context.Background(), map[string]any{}); !res.IsError {
		t.Error("missing url should error")
	}
	if res, _ := tool.Call(context.Background(), map[string]any{"url": "https://127.0.0.1/x", "method": "PURGE"}); !res.IsError {
		t.Error("unsupported method should error")
	}
	if res, _ := tool.Call(context.Background(), map[string]any{"url": "::bad url::"}); !res.IsError {
		t.Error("unparseable url should error")
	}
}

// ---- SSRF vetting of hostname targets (split-horizon DNS hole) ----

// TestHTTPRequestHostnameResolvesToPrivateRejected: an allowlisted hostname
// whose DNS answer is a private address must be refused BEFORE any dial — the
// string allowlist is not enough when split-horizon DNS rebinds the name.
func TestHTTPRequestHostnameResolvesToPrivateRejected(t *testing.T) {
	tool := toolWithResolver(t, []string{"api.example.com"}, fakeResolver{addrs: map[string][]net.IPAddr{
		"api.example.com": {{IP: net.ParseIP("10.0.0.5")}},
	}})
	res, err := tool.Call(context.Background(), map[string]any{"url": "https://api.example.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "resolves to") {
		t.Errorf("expected SSRF rejection of the private resolution, got %s", res.Content)
	}
}

// TestHTTPRequestHostnamePrivateAllowedByExplicitCIDR: an explicit CIDR rule
// is the operator's escape hatch for legitimately internal targets — the
// request proceeds, dialing exactly the vetted address (the pin path).
func TestHTTPRequestHostnamePrivateAllowedByExplicitCIDR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	url := "http://localhost" + strings.TrimPrefix(srv.URL, "http://127.0.0.1")

	tool := toolWithResolver(t, []string{"localhost", "127.0.0.0/8"}, fakeResolver{addrs: map[string][]net.IPAddr{
		"localhost": {{IP: net.ParseIP("127.0.0.1")}},
	}})
	res, err := tool.Call(context.Background(), map[string]any{"url": url})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("CIDR-allowed private target rejected: %s", res.Content)
	}
}

// TestHTTPRequestHostnameResolutionFailureRejected: a host that cannot be
// resolved fails closed — the vetting cannot run, so the target is refused.
func TestHTTPRequestHostnameResolutionFailureRejected(t *testing.T) {
	tool := toolWithResolver(t, []string{"api.example.com"}, fakeResolver{err: errors.New("dns down")})
	res, err := tool.Call(context.Background(), map[string]any{"url": "https://api.example.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "resolve") {
		t.Errorf("expected resolution-failure rejection, got %s", res.Content)
	}
}

// TestHTTPRequestWildcardStillVetted: even the explicit full-open "*" rule
// does not grant a private-address exemption — a hostname under "*" that
// resolves to loopback is still refused unless a CIDR rule allows it.
func TestHTTPRequestWildcardStillVetted(t *testing.T) {
	tool := toolWithResolver(t, []string{"*"}, fakeResolver{addrs: map[string][]net.IPAddr{
		"metadata.internal": {{IP: net.ParseIP("169.254.169.254")}},
	}})
	res, err := tool.Call(context.Background(), map[string]any{"url": "http://metadata.internal/latest"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "resolves to") {
		t.Errorf("expected SSRF rejection under the * rule, got %s", res.Content)
	}
}

// TestHTTPRequestRedirectToPrivateResolvingHostnameRejected: a redirect hop
// to an allowlisted hostname that resolves private is vetted and refused like
// the initial target — the 302 cannot smuggle past the SSRF guard.
func TestHTTPRequestRedirectToPrivateResolvingHostnameRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal.corp/secret", http.StatusFound)
	}))
	defer srv.Close()
	url := "http://localhost" + strings.TrimPrefix(srv.URL, "http://127.0.0.1")

	tool := toolWithResolver(t, []string{"localhost", "127.0.0.0/8", "internal.corp"}, fakeResolver{addrs: map[string][]net.IPAddr{
		"localhost":     {{IP: net.ParseIP("127.0.0.1")}},
		"internal.corp": {{IP: net.ParseIP("10.1.2.3")}},
	}})
	res, err := tool.Call(context.Background(), map[string]any{"url": url + "/start"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "resolves to") {
		t.Errorf("expected the private redirect hop to be refused, got %s", res.Content)
	}
}
