// Package netutil is the platform's small leaf of shared network
// helpers, kept free of any internal dependency so every package can
// import it without a cycle. It provides the embedded-IPv4 detection
// used by the SSRF guard (internal/webhook) and the http_request tool
// allowlist (internal/toolruntime/builtin).
package netutil

import "net"

// EmbeddedIPv4 returns the primary IPv4 reachable through an IPv6
// translation scheme — NAT64 (RFC 6052 well-known 64:ff9b::/96 or RFC 8215
// local-use 64:ff9b:1::/48), 6to4 (RFC 3056 2002:V4::/16), ISATAP (RFC 5214)
// or the legacy 4-in-6 ::a.b.c.d — or nil when ip carries no embedded
// address. It is the first candidate of EmbeddedIPv4s, so an address that
// several schemes could decode keeps the traditional reading (the /48 one).
// Guards that must not let a private target through should use
// EmbeddedIPv4s and fail closed.
func EmbeddedIPv4(ip net.IP) net.IP {
	v4s := EmbeddedIPv4s(ip)
	if len(v4s) == 0 {
		return nil
	}
	return v4s[0]
}

// EmbeddedIPv4s returns every IPv4 a translation scheme could reach from ip
// — the same schemes as EmbeddedIPv4 — or nil when ip carries no embedded
// address. A NAT64 local-use address is ambiguous: the identical bytes
// decode to different addresses under the /48 (PL=48) reading — IPv4 halves
// at bytes 6-7 and 9-10 around the reserved "u" octet at byte 8 — and the
// PL=64 reading — IPv4 at bytes 9-12, u at byte 8 — and which translator
// parameter the network uses is not observable from the address alone, so
// both candidates are returned (deduplicated). An address under the /48
// prefix whose bytes 10-11 spell the ISATAP marker (0x00005efe) is
// additionally reachable as IPv4 at bytes 12-15, so that reading is
// appended as a third candidate (deduplicated) too. The /48 reading stays
// first (primary); callers deciding allow/block must fail closed: refuse
// when any candidate is private, permit only when every candidate is.
// Per RFC 6052 §2.3 the u octet is removed unconditionally and translators
// do not enforce it being zero, so a u≠0 address must not be treated as
// non-embedded (that would let a private target disguised with a nonzero u
// octet slip past a caller's checks). 6to4 puts the IPv4 at bytes 2-5;
// ISATAP at bytes 12-15 (preceded by the 0x00005efe identifier marker at
// bytes 8-11); 4-in-6 in the low 32 bits with the high 96 bits zero.
func EmbeddedIPv4s(ip net.IP) []net.IP {
	v6 := ip.To16()
	if v6 == nil {
		return nil
	}
	// NAT64 well-known prefix 64:ff9b::/96: bytes 4-11 are the zero
	// prefix, the IPv4 is the low 32 bits.
	if v6[0] == 0 && v6[1] == 0x64 && v6[2] == 0xff && v6[3] == 0x9b {
		if v6[4] == 0 && v6[5] == 0 && v6[6] == 0 && v6[7] == 0 &&
			v6[8] == 0 && v6[9] == 0 && v6[10] == 0 && v6[11] == 0 {
			return []net.IP{net.IPv4(v6[12], v6[13], v6[14], v6[15])}
		}
		// Local-use prefix 64:ff9b:1::/48: IPv4 first half at bytes 6-7,
		// second half at bytes 9-10, straddling the "u" octet at byte 8,
		// plus the PL=64 reading of the same bytes (IPv4 at bytes 9-12).
		// Without the second candidate a PL=64-encoded target would be
		// mis-decoded (e.g. 64:ff9b:1:0:a:0:100:: reads 0.0.10.0 under
		// /48, but a PL=64 translator reaches 10.0.0.1) and could pass a
		// private-address check.
		if v6[4] == 0 && v6[5] == 1 {
			a := net.IPv4(v6[6], v6[7], v6[9], v6[10])
			b := net.IPv4(v6[9], v6[10], v6[11], v6[12])
			cands := []net.IP{a, b}
			if a.Equal(b) {
				cands = []net.IP{a}
			}
			// A /48 address whose bytes 10-11 spell the ISATAP marker
			// (0x00005efe) is also reachable as IPv4 at bytes 12-15 under
			// RFC 5214. The /48 and PL=64 readings of such an address can
			// both be public — e.g. 64:ff9b:1:0:0:5efe:a00:1 decodes to
			// the public-looking 0.0.0.94 and 0.94.254.10 — while the
			// ISATAP reading reaches a private target. Append it as a
			// third candidate (deduplicated) so fail-closed guards still
			// refuse it; the /48 prefix reading stays primary.
			if v6[10] == 0x5e && v6[11] == 0xfe {
				c := net.IPv4(v6[12], v6[13], v6[14], v6[15])
				for _, x := range cands {
					if x.Equal(c) {
						return cands
					}
				}
				return append(cands, c)
			}
			return cands
		}
		// An address under 64:ff9b::/32 that is neither /96 nor /48 may
		// still be ISATAP (RFC 5214): 64:ff9b::5efe:a.b.c.d reaches
		// a.b.c.d. The unconditional return below used to shadow the ISATAP
		// check, so bail out only when the 0x00005efe marker is absent and
		// let the ISATAP branch decode it.
		if v6[10] != 0x5e || v6[11] != 0xfe {
			return nil
		}
	}
	// 6to4 prefix 2002::/16 (RFC 3056): the IPv4 occupies bytes 2-5.
	if v6[0] == 0x20 && v6[1] == 0x02 {
		return []net.IP{net.IPv4(v6[2], v6[3], v6[4], v6[5])}
	}
	// Legacy 4-in-6 ::a.b.c.d: IPv4 in the low 32 bits of an address
	// whose high 96 bits are all zero.
	if v6[0] == 0 && v6[1] == 0 && v6[2] == 0 && v6[3] == 0 &&
		v6[4] == 0 && v6[5] == 0 && v6[6] == 0 && v6[7] == 0 &&
		v6[8] == 0 && v6[9] == 0 && v6[10] == 0 && v6[11] == 0 {
		return []net.IP{net.IPv4(v6[12], v6[13], v6[14], v6[15])}
	}
	// ISATAP (RFC 5214): the interface identifier is 0x0000_5EFE followed
	// by the IPv4 in the low 32 bits — bytes 8-11 are the 0x00005efe
	// marker and the IPv4 sits in bytes 12-15 (e.g.
	// 2001:db8::5efe:10.0.0.1). Any address carrying the 5efe marker is
	// decoded (fail-closed), even if the prefix half of the identifier is
	// nonzero. Unlike Teredo (RFC 4380) there are no server/obfuscation
	// bits to strip, so the embedded address is reachable as-is.
	if v6[10] == 0x5e && v6[11] == 0xfe {
		return []net.IP{net.IPv4(v6[12], v6[13], v6[14], v6[15])}
	}
	return nil
}
