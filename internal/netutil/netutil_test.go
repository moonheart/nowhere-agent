package netutil

import (
	"net"
	"testing"
)

func TestEmbeddedIPv4(t *testing.T) {
	cases := []struct {
		addr string
		want string // "" = expect nil
	}{
		// NAT64 well-known prefix 64:ff9b::/96.
		{"64:ff9b::a00:1", "10.0.0.1"},
		{"64:ff9b::808:808", "8.8.8.8"},
		// NAT64 local-use /48, u octet zero and non-zero.
		{"64:ff9b:1:a00:0:100::", "10.0.0.1"},
		{"64:ff9b:1:a00:101::", "10.0.1.0"},
		{"64:ff9b:1:808:108:800::", "8.8.8.8"},
		// NAT64 local-use under a PL=64 translator: the IPv4 sits at bytes
		// 9-12, so 64:ff9b:1:0:a:0:100:: reaches 10.0.0.1 even though the
		// /48 reading sees the public-looking 0.0.10.0. EmbeddedIPv4 keeps
		// the /48 reading primary; guards must fail closed over
		// EmbeddedIPv4s.
		{"64:ff9b:1:0:a:0:100::", "0.0.10.0"},
		{"64:ff9b:1:0:8:808:800::", "0.0.8.8"},
		// 6to4 (RFC 3056).
		{"2002:a00:1::", "10.0.0.1"},
		{"2002:808:808::", "8.8.8.8"},
		// ISATAP (RFC 5214).
		{"2001:db8::5efe:a00:1", "10.0.0.1"},
		{"2001:db8::5efe:808:808", "8.8.8.8"},
		{"::5efe:a00:1", "10.0.0.1"},
		// ISATAP marker under the NAT64 well-known prefix: not /96 or /48,
		// but bytes 10-11 still carry 0x5efe, so the embedded IPv4 at bytes
		// 12-15 is reachable and must be decoded (no early return).
		{"64:ff9b::5efe:a00:1", "10.0.0.1"},
		// /48 address whose identifier bytes also spell 0x5efe: the /48
		// reading stays primary (prefix priority), not the ISATAP reading.
		{"64:ff9b:1:0:0:5efe:a00:1", "0.0.0.94"},
		// Legacy 4-in-6.
		{"::a00:1", "10.0.0.1"},
		{"::808:808", "8.8.8.8"},
		// Plain addresses carry no embedded IPv4.
		{"2606:4700:4700::1111", ""},
		{"2001:db8::1", ""},
		{"8.8.8.8", ""},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.addr)
		if ip == nil {
			t.Fatalf("bad test address %q", c.addr)
		}
		got := EmbeddedIPv4(ip)
		if c.want == "" {
			if got != nil {
				t.Errorf("EmbeddedIPv4(%s) = %v, want nil", c.addr, got)
			}
			continue
		}
		if got == nil || !got.Equal(net.ParseIP(c.want)) {
			t.Errorf("EmbeddedIPv4(%s) = %v, want %s", c.addr, got, c.want)
		}
	}
}

// TestEmbeddedIPv4sCandidates pins the full candidate list, which guards use
// to fail closed: an ambiguous NAT64 local-use address yields both the /48
// and the PL=64 reading, so a private target under either reading is caught.
func TestEmbeddedIPv4sCandidates(t *testing.T) {
	cases := []struct {
		addr string
		want []string
	}{
		// /96: single, unambiguous.
		{"64:ff9b::a00:1", []string{"10.0.0.1"}},
		{"64:ff9b::808:808", []string{"8.8.8.8"}},
		// /48: both the /48 and the PL=64 reading; the /48 one first.
		{"64:ff9b:1:a00:0:100::", []string{"10.0.0.1", "0.1.0.0"}}, // PL=64 reading of 10.0.0.1@/48 is 0.1.0.0
		{"64:ff9b:1:0:a:0:100::", []string{"0.0.10.0", "10.0.0.1"}}, // PL=64 target: 10.0.0.1@bytes 9-12
		{"64:ff9b:1:0:8:808:800::", []string{"0.0.8.8", "8.8.8.8"}}, // PL=64 target: public 8.8.8.8
		// The two readings coincide: deduplicated to a single candidate.
		{"64:ff9b:1:a0a:a:a0a:a00::", []string{"10.10.10.10"}},
		// ISATAP under the NAT64 prefix: single.
		{"64:ff9b::5efe:a00:1", []string{"10.0.0.1"}},
		// 6to4 / 4-in-6: single.
		{"2002:a00:1::", []string{"10.0.0.1"}},
		{"::a00:1", []string{"10.0.0.1"}},
		// Plain addresses carry no embedded IPv4.
		{"2606:4700:4700::1111", nil},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.addr)
		if ip == nil {
			t.Fatalf("bad test address %q", c.addr)
		}
		got := EmbeddedIPv4s(ip)
		if len(c.want) == 0 {
			if len(got) != 0 {
				t.Errorf("EmbeddedIPv4s(%s) = %v, want nil", c.addr, got)
			}
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("EmbeddedIPv4s(%s) = %v, want %v", c.addr, got, c.want)
			continue
		}
		for i := range c.want {
			if !got[i].Equal(net.ParseIP(c.want[i])) {
				t.Errorf("EmbeddedIPv4s(%s)[%d] = %v, want %s", c.addr, i, got[i], c.want[i])
			}
		}
	}
}
