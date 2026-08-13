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
		// NAT64 local-use prefix 64:ff9b:1::/48, u octet zero and non-zero.
		{"64:ff9b:1:a00:0:100::", "10.0.0.1"},
		{"64:ff9b:1:a00:101::", "10.0.1.0"},
		{"64:ff9b:1:808:108:800::", "8.8.8.8"},
		// 6to4 (RFC 3056).
		{"2002:a00:1::", "10.0.0.1"},
		{"2002:808:808::", "8.8.8.8"},
		// ISATAP (RFC 5214).
		{"2001:db8::5efe:a00:1", "10.0.0.1"},
		{"2001:db8::5efe:808:808", "8.8.8.8"},
		{"::5efe:a00:1", "10.0.0.1"},
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
