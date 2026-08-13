// Package netutil is the platform's small leaf of shared network
// helpers, kept free of any internal dependency so every package can
// import it without a cycle. It provides the embedded-IPv4 detection
// used by the SSRF guard (internal/webhook) and the http_request tool
// allowlist (internal/toolruntime/builtin).
package netutil

import "net"

// EmbeddedIPv4 returns the IPv4 reachable through an IPv6 translation
// scheme — NAT64 (RFC 6052 well-known 64:ff9b::/96 or RFC 8215 local-use
// 64:ff9b:1::/48), 6to4 (RFC 3056 2002:V4::/16), ISATAP (RFC 5214) or the
// legacy 4-in-6 ::a.b.c.d — or nil when ip carries no embedded address.
// Per RFC 6052 §2.2 the /96 form carries the IPv4 in the low 32 bits; the
// /48 form splits it across the reserved "u" octet (bytes 6-7 and 9-10, u
// at byte 8, whose value is ignored: RFC 6052 §2.3 extraction removes the
// u octet unconditionally and translators do not enforce it being zero).
// 6to4 puts the IPv4 at bytes 2-5; ISATAP at bytes 12-15 (preceded by the
// 0x00005efe identifier marker at bytes 8-11); 4-in-6 in the low 32 bits
// with the high 96 bits zero.
func EmbeddedIPv4(ip net.IP) net.IP {
	v6 := ip.To16()
	if v6 == nil {
		return nil
	}
	// NAT64 well-known prefix 64:ff9b::/96: bytes 4-11 are the zero
	// prefix, the IPv4 is the low 32 bits.
	if v6[0] == 0 && v6[1] == 0x64 && v6[2] == 0xff && v6[3] == 0x9b {
		if v6[4] == 0 && v6[5] == 0 && v6[6] == 0 && v6[7] == 0 &&
			v6[8] == 0 && v6[9] == 0 && v6[10] == 0 && v6[11] == 0 {
			return net.IPv4(v6[12], v6[13], v6[14], v6[15])
		}
		// Local-use prefix 64:ff9b:1::/48: IPv4 first half at bytes 6-7,
		// second half at bytes 9-10, straddling the "u" octet at byte 8.
		// Any address under the /48 is decoded — per RFC 6052 §2.3 the u
		// octet is removed unconditionally, so a u≠0 address must not be
		// treated as non-embedded (that would let a private target
		// disguised with a nonzero u octet slip past a caller's checks).
		if v6[4] == 0 && v6[5] == 1 {
			return net.IPv4(v6[6], v6[7], v6[9], v6[10])
		}
		return nil
	}
	// 6to4 prefix 2002::/16 (RFC 3056): the IPv4 occupies bytes 2-5.
	if v6[0] == 0x20 && v6[1] == 0x02 {
		return net.IPv4(v6[2], v6[3], v6[4], v6[5])
	}
	// Legacy 4-in-6 ::a.b.c.d: IPv4 in the low 32 bits of an address
	// whose high 96 bits are all zero.
	if v6[0] == 0 && v6[1] == 0 && v6[2] == 0 && v6[3] == 0 &&
		v6[4] == 0 && v6[5] == 0 && v6[6] == 0 && v6[7] == 0 &&
		v6[8] == 0 && v6[9] == 0 && v6[10] == 0 && v6[11] == 0 {
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	}
	// ISATAP (RFC 5214): the interface identifier is 0x0000_5EFE followed
	// by the IPv4 in the low 32 bits — bytes 8-11 are the 0x00005efe
	// marker and the IPv4 sits in bytes 12-15 (e.g.
	// 2001:db8::5efe:10.0.0.1). Any address carrying the 5efe marker is
	// decoded (fail-closed), even if the prefix half of the identifier is
	// nonzero. Unlike Teredo (RFC 4380) there are no server/obfuscation
	// bits to strip, so the embedded address is reachable as-is.
	if v6[10] == 0x5e && v6[11] == 0xfe {
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	}
	return nil
}
