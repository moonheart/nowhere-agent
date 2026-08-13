package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		"http://127.0.0.1:8080/hook",          // literal loopback refused even without DNS
		"http://10.1.2.3/hook",                // literal private refused
		"http://0.0.0.5/hook",                 // 0.0.0.0/8 "this network" (RFC 6890): kernel-routed to localhost
		"http://[::1]/hook",                   // IPv6 loopback refused
		"http://[64:ff9b::a00:1]/hook",        // NAT64 (RFC 6052) embedding 10.0.0.1 refused
		"http://[64:ff9b:1:a00:0:100::]/hook", // NAT64 local-use /48 embedding 10.0.0.1
		"http://[64:ff9b:1:a00:101::]/hook",   // NAT64 local-use /48, u octet ≠ 0, embedding 10.0.1.0
		"http://[64:ff9b:1:0:a:0:100::]/hook",  // NAT64 PL=64: 10.0.0.1 at bytes 9-12 (the /48 reading sees public 0.0.10.0)
		"http://[64:ff9b:1:0:0:5efe:a00:1]/hook", // NAT64 /48 + ISATAP: bytes 10-11 are 5efe, 10.0.0.1 at bytes 12-15 while /48 readings see public 0.0.0.94/0.94.254.10 — fail closed
		"http://[2002:a00:1::]/hook",          // 6to4 (RFC 3056) embedding 10.0.0.1 refused
		"http://[2001:db8::5efe:a00:1]/hook",  // ISATAP (RFC 5214) embedding 10.0.0.1 refused
		"http://[::a00:1]/hook",               // 4-in-6 embedding 10.0.0.1 refused
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
		"http://0.0.0.1/x",    // 0.0.0.0/8 "this network": localhost reachable via lo
		"http://0.0.0.255/x",  // last address of the /8, not just 0.0.0.0
		"http://10.0.0.1/x",
		"http://172.16.0.1/x",
		"http://192.168.1.1/x",
		"http://169.254.169.254/latest/meta-data", // cloud metadata
		"http://100.64.0.1/x",                     // CGNAT
		"http://[::1]/x",
		"http://[fc00::1]/x",
		"http://[64:ff9b::a00:1]/x",        // NAT64 well-known prefix embedding 10.0.0.1
		"http://[64:ff9b:1:a00:0:100::]/x", // NAT64 local-use /48 embedding 10.0.0.1
		"http://[64:ff9b:1:a00:101::]/x",   // NAT64 local-use /48, u octet ≠ 0, embedding 10.0.1.0
		"http://[64:ff9b:1:0:a:0:100::]/x", // NAT64 PL=64: 10.0.0.1 at bytes 9-12, /48 reading public 0.0.10.0 — fail closed
		"http://[64:ff9b:1:0:8:808:800::]/x", // NAT64 PL=64: 8.8.8.8 at bytes 9-12, but the /48 reading 0.0.8.8 is 0.0.0.0/8 (localhost-routed) — fail closed
		"http://[64:ff9b:1:0:0:5efe:a00:1]/x", // NAT64 /48 + ISATAP: 10.0.0.1 at bytes 12-15, /48 readings public — fail closed
		"http://[2002:a00:1::]/x",          // 6to4 embedding 10.0.0.1
		"http://[2001:db8::5efe:a00:1]/x",  // ISATAP embedding 10.0.0.1
		"http://[::a00:1]/x",               // 4-in-6 embedding 10.0.0.1
		"http://localhost:8080/x",          // resolves loopback via real DNS
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
		"https://[2606:4700:4700::1111]/x",  // Cloudflare DNS
		"http://[64:ff9b::808:808]/x",       // NAT64 embedding public 8.8.8.8
		"http://[64:ff9b:1:808:8:800::]/x",  // NAT64 local-use /48 embedding public 8.8.8.8
		"http://[2002:808:808::]/x",         // 6to4 embedding public 8.8.8.8
		"http://[2001:db8::5efe:808:808]/x", // ISATAP embedding public 8.8.8.8
		"http://[::808:808]/x",              // 4-in-6 embedding public 8.8.8.8
	} {
		if err := g.CheckURL(ctx, u); err != nil {
			t.Errorf("CheckURL(%q): %v, want ok", u, err)
		}
	}
}

func TestCheckURLDNSPolicy(t *testing.T) {
	g := testGuard(t, nil, nil)
	g.resolver = stubResolver{hosts: map[string][]net.IPAddr{
		"public.example":       {{IP: net.ParseIP("93.184.216.34")}},
		"rebinding.example":    {{IP: net.ParseIP("169.254.169.254")}},
		"private.example":      {{IP: net.ParseIP("10.1.2.3")}},
		"mixed.example":        {{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}},
		"nat64.example":        {{IP: net.ParseIP("64:ff9b::a00:1")}}, // DNS64 answer for 10.0.0.1
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
		{"http://mixed.example/h", false},        // any private address refuses the target
		{"http://nat64.example/h", false},        // DNS64 NAT64 answer for a private IPv4
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
		{"http://10.1.2.3/h", true},                   // allowlisted CIDR
		{"http://172.16.0.5/h", true},                 // allowlisted CIDR
		{"http://im.example.internal/h", true},        // allowlisted hostname
		{"http://[64:ff9b::a00:1]/h", true},           // NAT64 well-known prefix embedding allowlisted 10.0.0.1
		{"http://[64:ff9b:1:a00:0:100::]/h", true},    // NAT64 local-use /48 embedding allowlisted 10.0.0.1
		{"http://[64:ff9b:1:a00:101::]/h", true},      // NAT64 local-use /48, u octet ≠ 0, embedding allowlisted 10.0.1.0
		{"http://[64:ff9b:1:0:a:0:100::]/h", true},     // NAT64 PL=64: 10.0.0.1 at bytes 9-12 matches the allowlisted CIDR
		{"http://[64:ff9b:1:0:0:5efe:a00:1]/h", true},  // NAT64 /48 + ISATAP: 10.0.0.1 at bytes 12-15 matches the allowlisted CIDR
		{"http://[2002:a00:1::]/h", true},             // 6to4 embedding allowlisted 10.0.0.1
		{"http://[2001:db8::5efe:a00:1]/h", true},     // ISATAP embedding allowlisted 10.0.0.1
		{"http://[::a00:1]/h", true},                  // 4-in-6 embedding allowlisted 10.0.0.1
		{"http://192.168.1.1/h", false},               // outside the CIDRs
		{"http://[64:ff9b::c0a8:101]/h", false},       // NAT64 embedding 192.168.1.1, outside the CIDRs
		{"http://[2002:c0a8:101::]/h", false},         // 6to4 embedding 192.168.1.1, outside the CIDRs
		{"http://[2001:db8::5efe:c0a8:101]/h", false}, // ISATAP embedding 192.168.1.1, outside the CIDRs
		{"http://[::c0a8:101]/h", false},              // 4-in-6 embedding 192.168.1.1, outside the CIDRs
		{"http://other.example/h", false},             // private address, host not allowlisted
		{"http://127.0.0.1/h", false},                 // loopback is never allowed by CIDR
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

// TestCheckRedirectBlocksPrivateHop proves a redirect hop is validated with
// the same guard as the initial target: a public first hop that 302s to
// loopback is refused.
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

// TestCheckRedirectPinsValidatedHop proves a redirect hop is not only
// validated but pinned: after CheckRedirect the hop request carries the
// vetted address set in its context, so the hop's dial cannot be re-resolved
// to a rebinding address either.
func TestCheckRedirectPinsValidatedHop(t *testing.T) {
	g := testGuard(t, nil, nil)
	g.resolver = stubResolver{hosts: map[string][]net.IPAddr{
		"hop.example": {{IP: net.ParseIP("8.8.8.8")}},
	}}
	req := httptest.NewRequest("GET", "http://hop.example/hook", nil)
	via := []*http.Request{httptest.NewRequest("GET", "http://8.8.8.8/x", nil)}
	if err := g.CheckRedirect(req, via); err != nil {
		t.Fatalf("public redirect hop refused: %v", err)
	}
	p, ok := req.Context().Value(pinnedIPKey{}).(pinnedDial)
	if !ok {
		t.Fatal("redirect hop not pinned in request context")
	}
	if p.host != "hop.example" || len(p.ips) != 1 || !p.ips[0].Equal(net.ParseIP("8.8.8.8")) {
		t.Fatalf("unexpected pin for redirect hop: %+v", p)
	}
}

// TestDialContextFallbackOnHostMismatch proves a dial whose address does not
// match the pinned host (an HTTP proxy from the environment, say) falls back
// to a plain dial instead of pinning the wrong address: the pin points at an
// unreachable TEST-NET address, yet the local listener is reached because the
// dial is for a different host than the pin.
func TestDialContextFallbackOnHostMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	g := testGuard(t, nil, nil)
	ctx := withPinned(context.Background(), pinnedDial{host: "target.example", ips: []net.IP{net.ParseIP("192.0.2.1")}})
	conn, err := g.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("mismatched pin must fall back to a plain dial: %v", err)
	}
	conn.Close()
}

// TestDeliverPinsValidatedAddress proves the delivery dials exactly the
// address the guard vetted: the stub resolver reports a PUBLIC address for
// "localhost", so the guard vets it and delivery is permitted — but the dial
// must go to that vetted address, never to the local listener that the real
// (system) resolution of localhost would reach. A rebinding host that flips
// to loopback after the check therefore cannot receive the delivery.
func TestDeliverPinsValidatedAddress(t *testing.T) {
	received := make(chan struct{}, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case received <- struct{}{}:
			default:
			}
			conn.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	g := testGuard(t, nil, nil)
	g.resolver = stubResolver{hosts: map[string][]net.IPAddr{
		"localhost": {{IP: net.ParseIP("8.8.8.8")}},
	}}
	n := New(Options{SSRF: g, Timeout: 2 * time.Second, Logger: testLogger(t)})
	// The hostname vets as public, so delivery is permitted; the pinned
	// 8.8.8.8 is unreachable, so the delivery fails — and it must fail
	// without ever dialing the local listener sitting at the same port.
	err = n.Deliver(context.Background(),
		fmt.Sprintf("http://localhost:%d/hook", port),
		RunCompletedPayload{Event: "run.completed"})
	if err == nil {
		t.Fatal("delivery to an unreachable pinned address succeeded")
	}
	select {
	case <-received:
		t.Fatal("delivery dialed the local listener instead of the pinned address")
	default:
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

// TestDeliverSignsPayload proves the HMAC signature header: with a signing
// secret configured, the consumer can verify the body's authenticity.
func TestDeliverSignsPayload(t *testing.T) {
	const secret = "webhook-signing-secret"
	sigCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigCh <- r.Header.Get("X-Nowhere-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := testGuard(t, []string{"127.0.0.0/8"}, nil)
	n := New(Options{SSRF: g, Timeout: 2 * time.Second, SigningSecret: secret, Logger: testLogger(t)})
	payload := RunCompletedPayload{Event: "run.completed", RunID: "r1", Status: "done"}
	if err := n.Deliver(context.Background(), srv.URL+"/hook", payload); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	got := <-sigCh
	if !strings.HasPrefix(got, "sha256=") {
		t.Fatalf("signature header = %q, want sha256= prefix", got)
	}
	// Recompute the expected signature over the same JSON the notifier sent.
	body, _ := json.Marshal(payload)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(got), []byte(want)) {
		t.Fatalf("signature %q != expected %q", got, want)
	}
	// A wrong secret must not verify.
	bad := hmac.New(sha256.New, []byte("wrong"))
	bad.Write(body)
	if hmac.Equal([]byte(got), []byte("sha256="+hex.EncodeToString(bad.Sum(nil)))) {
		t.Fatal("signature verifies under the wrong secret")
	}
}

// TestDeliverUnsignedWithoutSecret proves no signature header is sent when no
// signing secret is configured (legacy behavior).
func TestDeliverUnsignedWithoutSecret(t *testing.T) {
	sigCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigCh <- r.Header.Get("X-Nowhere-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	g := testGuard(t, []string{"127.0.0.0/8"}, nil)
	n := New(Options{SSRF: g, Timeout: 2 * time.Second, Logger: testLogger(t)})
	if err := n.Deliver(context.Background(), srv.URL+"/hook", RunCompletedPayload{Event: "run.completed"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := <-sigCh; got != "" {
		t.Fatalf("signature header present without a secret: %q", got)
	}
}
