package webhook

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubResolver returns a fixed address set per host, no real DNS.
type stubResolver struct {
	hosts map[string][]net.IPAddr
}

func (s stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return s.hosts[host], nil
}

func testGuard(t *testing.T, allowCIDRs, allowHosts []string) *Guard {
	t.Helper()
	g, err := NewGuard(allowCIDRs, allowHosts)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestValidateURL(t *testing.T) {
	g := testGuard(t, nil, nil)
	for _, bad := range []string{
		"", "file:///etc/passwd", "javascript:alert(1)", "ftp://x/y",
		"https://", "localhost", "/relative", "http://",
		"http://127.0.0.1:8080/hook", // literal loopback refused even without DNS
		"http://10.1.2.3/hook",       // literal private refused
		"http://[::1]/hook",          // IPv6 loopback refused
	} {
		if err := g.ValidateURL(bad); err == nil {
			t.Errorf("ValidateURL(%q): accepted, want block", bad)
		}
	}
	for _, good := range []string{
		"https://hooks.example.com/x", "http://8.8.8.8/hook", "http://1.1.1.1:8080/h",
	} {
		if err := g.ValidateURL(good); err != nil {
			t.Errorf("ValidateURL(%q): %v, want ok", good, err)
		}
	}
}

func TestCheckURLPrivateTargets(t *testing.T) {
	g := testGuard(t, nil, nil)
	ctx := context.Background()
	for _, u := range []string{
		"http://127.0.0.1:1/x",
		"http://10.0.0.1/x",
		"http://172.16.0.1/x",
		"http://192.168.1.1/x",
		"http://169.254.169.254/latest/meta-data", // cloud metadata
		"http://100.64.0.1/x",                     // CGNAT
		"http://[::1]/x",
		"http://[fc00::1]/x",
		"http://localhost:8080/x", // resolves loopback via real DNS
	} {
		if err := g.CheckURL(ctx, u); err == nil {
			t.Errorf("CheckURL(%q): accepted, want block", u)
		}
	}
}

func TestCheckURLPublicTargets(t *testing.T) {
	g := testGuard(t, nil, nil)
	ctx := context.Background()
	for _, u := range []string{
		"https://example.com/hook", "http://8.8.8.8/x", "http://1.1.1.1:8080/x",
		"https://[2606:4700:4700::1111]/x", // Cloudflare DNS
	} {
		if err := g.CheckURL(ctx, u); err != nil {
			t.Errorf("CheckURL(%q): %v, want ok", u, err)
		}
	}
}

func TestCheckURLDNSPolicy(t *testing.T) {
	g := testGuard(t, nil, nil)
	g.resolver = stubResolver{hosts: map[string][]net.IPAddr{
		"public.example":     {{IP: net.ParseIP("93.184.216.34")}},
		"rebinding.example":  {{IP: net.ParseIP("169.254.169.254")}},
		"private.example":    {{IP: net.ParseIP("10.1.2.3")}},
		"mixed.example":      {{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}},
		"unresolvable.example": {},
	}}
	ctx := context.Background()
	for _, tc := range []struct {
		url  string
		want bool // true = allowed
	}{
		{"http://public.example/h", true},
		{"http://rebinding.example/h", false},
		{"http://private.example/h", false},
		{"http://mixed.example/h", false}, // any private address refuses the target
		{"http://unresolvable.example/h", false}, // fail-closed
	} {
		err := g.CheckURL(ctx, tc.url)
		if tc.want && err != nil {
			t.Errorf("%s: %v, want ok", tc.url, err)
		}
		if !tc.want && err == nil {
			t.Errorf("%s: accepted, want block", tc.url)
		}
	}
}

func TestAllowlistOpensPrivateTargets(t *testing.T) {
	g := testGuard(t, []string{"10.0.0.0/8", "172.16.0.0/12"}, []string{"im.example.internal"})
	g.resolver = stubResolver{hosts: map[string][]net.IPAddr{
		"im.example.internal": {{IP: net.ParseIP("192.168.5.10")}},
		"other.example":       {{IP: net.ParseIP("192.168.5.20")}},
	}}
	ctx := context.Background()
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"http://10.1.2.3/h", true},        // allowlisted CIDR
		{"http://172.16.0.5/h", true},      // allowlisted CIDR
		{"http://im.example.internal/h", true}, // allowlisted hostname
		{"http://192.168.1.1/h", false},    // outside the CIDRs
		{"http://other.example/h", false},  // private address, host not allowlisted
		{"http://127.0.0.1/h", false},      // loopback is never allowed by CIDR
	} {
		err := g.CheckURL(ctx, tc.url)
		if tc.want && err != nil {
			t.Errorf("%s: %v, want ok", tc.url, err)
		}
		if !tc.want && err == nil {
			t.Errorf("%s: accepted, want block", tc.url)
		}
	}
}

func TestNewGuardRejectsBadCIDR(t *testing.T) {
	if _, err := NewGuard([]string{"not-a-cidr"}, nil); err == nil {
		t.Fatal("bad CIDR accepted")
	}
}

func TestCheckRedirectBlocksPrivateHop(t *testing.T) {
	g := testGuard(t, nil, nil)
	// First hop public, redirect target loopback: the hop must be refused.
	req := httptest.NewRequest("GET", "http://127.0.0.1:1/x", nil)
	err := g.CheckRedirect(req, []*http.Request{httptest.NewRequest("GET", "http://8.8.8.8/x", nil)})
	if err == nil {
		t.Fatal("redirect to loopback accepted")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("redirect error should name the guard: %v", err)
	}
}

// TestDeliverBlocksPrivateTarget proves the guard runs in the delivery path,
// before any connection is attempted (127.0.0.1 would otherwise dial).
func TestDeliverBlocksPrivateTarget(t *testing.T) {
	g := testGuard(t, nil, nil)
	n := New(Options{SSRF: g, Logger: testLogger(t)})
	err := n.Deliver(context.Background(), "http://127.0.0.1:1/hook", RunCompletedPayload{Event: "run.completed"})
	if err == nil {
		t.Fatal("deliver to loopback succeeded")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("error should name the guard: %v", err)
	}
}

// TestDeliverAllowsPublicTarget proves the guard lets public deliveries
// through to the (local test) consumer.
func TestDeliverAllowsPublicTarget(t *testing.T) {
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// The httptest server listens on 127.0.0.1 — allow it via CIDR so the
	// guard permits it, mirroring an intranet consumer.
	g := testGuard(t, []string{"127.0.0.0/8"}, nil)
	n := New(Options{SSRF: g, Timeout: 2 * time.Second, Logger: testLogger(t)})
	if err := n.Deliver(context.Background(), srv.URL+"/hook", RunCompletedPayload{Event: "run.completed"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer never received the delivery")
	}
}
