package trustedproxy

import (
	"net/http"
	"testing"
)

func TestEmptySetNeverTrustsHeaders(t *testing.T) {
	s := New(nil)
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.7")
	h.Set("X-Real-IP", "198.51.100.2")
	if got := s.ClientIP("10.0.0.5:1234", h); got != "10.0.0.5" {
		t.Fatalf("empty set must fall back to peer, got %q", got)
	}
}

func TestTrustedPeerHonoursXFFThenXRealIP(t *testing.T) {
	s := New([]string{"10.0.0.0/8", "2001:db8::/32"})
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.7, 70.41.3.18")
	if got := s.ClientIP("10.0.0.5:1234", h); got != "203.0.113.7" {
		t.Fatalf("trusted peer should take first XFF hop, got %q", got)
	}

	h2 := http.Header{}
	h2.Set("X-Real-IP", "198.51.100.2")
	if got := s.ClientIP("10.0.0.5:1234", h2); got != "198.51.100.2" {
		t.Fatalf("trusted peer should honour X-Real-IP, got %q", got)
	}
}

func TestUntrustedPeerIgnoresHeadersEvenWhenListedCIDRExists(t *testing.T) {
	s := New([]string{"10.0.0.0/8"})
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.7")
	if got := s.ClientIP("192.168.1.9:9999", h); got != "192.168.1.9" {
		t.Fatalf("peer outside the trusted set must not be honoured, got %q", got)
	}
}

func TestIPv6(t *testing.T) {
	s := New([]string{"2001:db8::/32"})
	h := http.Header{}
	h.Set("X-Forwarded-For", "2001:db8::99")
	if got := s.ClientIP("[2001:db8::1]:443", h); got != "2001:db8::99" {
		t.Fatalf("trusted IPv6 proxy should forward XFF, got %q", got)
	}
	if got := s.ClientIP("[2001:db8::1]:443", http.Header{}); got != "2001:db8::1" {
		t.Fatalf("trusted peer without headers falls back to peer, got %q", got)
	}
	untrusted := New(nil)
	if got := untrusted.ClientIP("[2001:db8::1]:443", h); got != "2001:db8::1" {
		t.Fatalf("untrusted IPv6 peer must be the client, got %q", got)
	}
}

func TestBareHostWithoutPort(t *testing.T) {
	s := New([]string{"10.0.0.0/8"})
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.7")
	if got := s.ClientIP("10.0.0.5", h); got != "203.0.113.7" {
		t.Fatalf("portless trusted peer should still forward XFF, got %q", got)
	}
}

func TestNewSkipsInvalidEntries(t *testing.T) {
	// Invalid CIDRs and bare garbage must be dropped, never broaden trust.
	s := New([]string{"not-a-cidr", "10.0.0.0/", "999.1.1.1", "10.1.0.0/8", ""})
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.7")
	if got := s.ClientIP("10.2.0.1:80", h); got != "203.0.113.7" {
		t.Fatalf("valid entry in a partially bad list must still work, got %q", got)
	}
	s2 := New([]string{"bogus"})
	if got := s2.ClientIP("10.2.0.1:80", h); got != "10.2.0.1" {
		t.Fatalf("all-invalid list must trust nothing, got %q", got)
	}
}

func TestBareIPEntryIsHostPrefix(t *testing.T) {
	// "10.0.0.9" alone means the /32, not the whole /8.
	s := New([]string{"10.0.0.9"})
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.7")
	if got := s.ClientIP("10.0.0.9:1234", h); got != "203.0.113.7" {
		t.Fatalf("exact host should be trusted, got %q", got)
	}
	if got := s.ClientIP("10.0.0.10:1234", h); got != "10.0.0.10" {
		t.Fatalf("neighbour must not be trusted by a host prefix, got %q", got)
	}
}

func TestClientIPUsesProcessDefault(t *testing.T) {
	SetDefault([]string{"10.0.0.0/8"})
	defer SetDefault(nil)
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.7")
	if got := ClientIP("10.0.0.5:1234", h); got != "203.0.113.7" {
		t.Fatalf("process default set should apply, got %q", got)
	}
	SetDefault(nil)
	if got := ClientIP("10.0.0.5:1234", h); got != "10.0.0.5" {
		t.Fatalf("cleared default must not honour headers, got %q", got)
	}
}
