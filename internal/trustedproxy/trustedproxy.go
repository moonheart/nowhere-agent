// Package trustedproxy resolves the effective client IP of an inbound request,
// honouring reverse-proxy headers (X-Forwarded-For, X-Real-IP) ONLY when the
// direct socket peer falls inside a configured trusted-proxy CIDR set. The
// secure default is an empty set: no header is ever trusted, and the peer
// address is the client. Blindly trusting X-Forwarded-For lets any caller
// spoof the IP that rate limiting and the audit trail key on.
package trustedproxy

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Set is an immutable set of trusted proxy CIDR prefixes. The zero value
// trusts nothing (all ClientIP lookups fall back to the peer address).
type Set struct {
	prefixes []netip.Prefix
}

// New builds a Set from CIDR strings ("10.0.0.0/8", "::1/128"). Unparseable
// entries are skipped — a typo must not silently broaden trust — and a bare IP
// is treated as a host prefix (/32 or /128). An empty slice yields a set that
// trusts nothing.
func New(cidrs []string) *Set {
	s := &Set{}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(c, "/") {
			parsed, err := netip.ParsePrefix(c)
			if err != nil {
				continue
			}
			p = parsed
		} else if a, err := netip.ParseAddr(c); err == nil {
			p = netip.PrefixFrom(a, a.BitLen())
		} else {
			continue
		}
		s.prefixes = append(s.prefixes, p.Masked())
	}
	return s
}

// contains reports whether addr falls inside any trusted prefix.
func (s *Set) contains(addr netip.Addr) bool {
	for _, p := range s.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIP returns the effective client IP for a peer given by remoteAddr
// (host:port) and the request headers. When the peer falls inside a trusted
// prefix, the first X-Forwarded-For hop wins, then X-Real-IP; otherwise — or
// when neither header is present — the peer host itself is the client. An
// empty set never honours either header.
func (s *Set) ClientIP(remoteAddr string, h http.Header) string {
	peer := strings.Trim(remoteAddr, "[]")
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		peer = host
	}
	if addr, err := netip.ParseAddr(peer); err != nil || !s.contains(addr) {
		return peer
	}
	if xff := h.Get("X-Forwarded-For"); xff != "" {
		if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if xr := strings.TrimSpace(h.Get("X-Real-IP")); xr != "" {
		return xr
	}
	return peer
}

// defaultSet is the process-wide trust configuration, set once at startup from
// HTTP_TRUSTED_PROXY_CIDRS. The empty default honours no proxy headers until an
// operator opts in — the secure posture.
var defaultSet = New(nil)

// SetDefault replaces the process-wide trusted-proxy set. Called once at
// startup from configuration; tests restore the previous value.
func SetDefault(cidrs []string) { defaultSet = New(cidrs) }

// Default returns the process-wide trusted-proxy set.
func Default() *Set { return defaultSet }

// ClientIP resolves the client IP using the process-wide trusted-proxy set.
func ClientIP(remoteAddr string, h http.Header) string {
	return defaultSet.ClientIP(remoteAddr, h)
}
