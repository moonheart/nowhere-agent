package builtin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

	allow, _ := Allowlist([]string{"127.0.0.1"})
	tool := NewHTTPRequest(allow, 10*time.Second)
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
	allow, _ := Allowlist([]string{"api.example.com"})
	tool := NewHTTPRequest(allow, 10*time.Second)
	res, err := tool.Call(context.Background(), map[string]any{"url": "https://evil.example.net/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "allowlist") {
		t.Errorf("expected allowlist rejection, got %s", res.Content)
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

	allow, _ := Allowlist([]string{"127.0.0.1"})
	tool := NewHTTPRequest(allow, 10*time.Second)
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
	allow, _ := Allowlist([]string{"127.0.0.1"})
	tool := NewHTTPRequest(allow, 10*time.Second)

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
