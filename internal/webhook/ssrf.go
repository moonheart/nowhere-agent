// SSRF guard for outbound webhook delivery. Webhook URLs come from
// configurable, user-written sources (scheduled-task webhook_url, inbound
// webhook notify_url, the global WEBHOOK_URL), so a misconfigured or hostile
// URL can otherwise make the platform's server POST into its own private
// network — the classic server-side-request-forgery. The guard runs at
// delivery time, on every attempt, right before any connection is made:
//
//   - ValidateURL rejects anything that is not an absolute http(s) URL.
//   - CheckURL additionally resolves the host and rejects the target when any
//     resolved address is loopback/private/link-local/multicast — unless the
//     host or the address is explicitly allowlisted (the escape hatch for
//     legitimately internal notification targets: an intranet IM gateway, a
//     workflow engine on a private subnet, …).
//   - CheckRedirect re-validates every redirect hop with the same rules, so a
//     public URL cannot smuggle the request to a private address via a 302.
//
// This mirrors the guard langgraph_api's webhook module applies
// (allowed_domains/allowed_ports/loopback interception), applied here at the
// single delivery choke point so every webhook source inherits it.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ErrBlocked is returned when a delivery target is refused by the guard.
var ErrBlocked = errors.New("webhook: delivery target blocked by SSRF guard")

// Guard blocks outbound delivery to private/loopback network targets.
type Guard struct {
	resolver   netResolver // injectable for tests
	allowNets  []*net.IPNet
	allowHosts map[string]struct{}
}

// netResolver is the DNS surface the guard needs; *net.Resolver satisfies it.
type netResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// NewGuard builds a Guard. allowCIDRs and allowHosts are the escape hatch for
// legitimately internal targets: CIDR blocks (e.g. "10.0.0.0/8") and exact
// hostnames ("im.example.internal"), any of which permit delivery even to a
// private address. A malformed CIDR is an error, not a silent skip — a typo'd
// allowlist must not quietly disable the guard.
func NewGuard(allowCIDRs, allowHosts []string) (*Guard, error) {
	g := &Guard{
		resolver:   net.DefaultResolver,
		allowHosts: make(map[string]struct{}, len(allowHosts)),
	}
	for _, h := range allowHosts {
		h = strings.TrimSpace(h)
		if h != "" {
			g.allowHosts[strings.ToLower(h)] = struct{}{}
		}
	}
	for _, cidr := range allowCIDRs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, net, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("webhook SSRF allowlist: bad CIDR %q: %w", cidr, err)
		}
		g.allowNets = append(g.allowNets, net)
	}
	return g, nil
}

// ValidateURL is the write-time, DNS-free check: an absolute http(s) URL with
// a host. Anything else is refused so a target can never be a non-HTTP scheme.
func (g *Guard) ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return ErrBlocked
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrBlocked
	}
	host := hostnameOnly(u.Host)
	// A literal IP target needs no DNS and is checked directly.
	if ip := net.ParseIP(host); ip != nil {
		if g.allowedIP(ip, "") {
			return nil
		}
		return ErrBlocked
	}
	return nil
}

// CheckURL is the delivery-time check: ValidateURL plus DNS resolution, where
// any resolved private/loopback/link-local/multicast address is refused unless
// allowlisted.
func (g *Guard) CheckURL(ctx context.Context, raw string) error {
	if err := g.ValidateURL(raw); err != nil {
		return err
	}
	u, _ := url.Parse(raw)
	host := hostnameOnly(u.Host)
	if ip := net.ParseIP(host); ip != nil {
		// Literal IP: already decided by ValidateURL via allowedIP.
		if !g.allowedIP(ip, "") {
			return ErrBlocked
		}
		return nil
	}
	// Allowlisted hostname bypasses resolution entirely (an internal name may
	// not resolve on the platform's public DNS, which is exactly why the
	// operator allowlisted it).
	if _, ok := g.allowHosts[strings.ToLower(host)]; ok {
		return nil
	}
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		// A resolution failure is refused, not passed through: the target
		// cannot be vetted, and failing closed is the safe side.
		return fmt.Errorf("%w: resolve %q: %v", ErrBlocked, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %q resolved to no addresses", ErrBlocked, host)
	}
	for _, a := range addrs {
		if !g.allowedIP(a.IP, host) {
			return fmt.Errorf("%w: %q resolves to %s", ErrBlocked, host, a.IP)
		}
	}
	return nil
}

// CheckRedirect re-validates every redirect hop (installed as the http
// client's CheckRedirect). Without it a public URL could 302 the request into
// the private network.
func (g *Guard) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("webhook: too many redirects")
	}
	return g.CheckURL(req.Context(), req.URL.String())
}

// allowedIP reports whether the address may be delivered to: public addresses
// always, private/loopback/etc. only when the address or the original host is
// allowlisted.
func (g *Guard) allowedIP(ip net.IP, host string) bool {
	if !isBlockedIP(ip) {
		return true
	}
	if len(g.allowNets) == 0 {
		return false
	}
	for _, n := range g.allowNets {
		if n.Contains(ip) {
			return true
		}
	}
	// Translation schemes reach an embedded IPv4 on the caller's behalf, so
	// an allowlist keyed on that address must match too — otherwise a
	// private target smuggled as [64:ff9b::10.0.0.1] evades a 10.0.0.0/8
	// allowlist that only sees the raw IPv6.
	if v4 := nat64IPv4(ip); v4 != nil {
		for _, n := range g.allowNets {
			if n.Contains(v4) {
				return true
			}
		}
	}
	_, hostAllowed := g.allowHosts[strings.ToLower(host)]
	return hostAllowed
}

// isBlockedIP reports whether ip is loopback, private, link-local, multicast,
// unspecified, or otherwise non-public.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return !public4(v4)
	}
	return !public6(ip)
}

func public4(ip net.IP) bool {
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified(), ip.Equal(net.IPv4zero):
		return false
	}
	// 100.64.0.0/10 (CGNAT) and 192.0.0.0/24 (IETF protocol assignments).
	if ip[0] == 100 && ip[1]&0xC0 == 64 {
		return false
	}
	if ip[0] == 192 && ip[1] == 0 {
		return false
	}
	return true
}

func public6(ip net.IP) bool {
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified():
		return false
	}
	// NAT64 addresses embed an IPv4 that the translator reaches on the
	// caller's behalf: vet THAT address, or a private target smuggled as
	// [64:ff9b::10.0.0.1] would slip past the IPv6 checks (IsPrivate etc.
	// do not cover the RFC 6052 well-known prefix).
	if v4 := nat64IPv4(ip); v4 != nil {
		return public4(v4)
	}
	return true
}

// nat64IPv4 returns the IPv4 embedded in an RFC 6052 NAT64 IPv6 address
// (well-known 64:ff9b::/96 or local-use 64:ff9b:1::/48, RFC 8215), or nil.
// Per RFC 6052 §2.2 the /96 form carries the IPv4 in the low 32 bits; the
// /48 form splits it across the reserved "u" octet (bytes 6-7 and 9-10, u
// at byte 8 MUST be zero).
func nat64IPv4(ip net.IP) net.IP {
	v6 := ip.To16()
	if v6 == nil || v6[0] != 0 || v6[1] != 0x64 || v6[2] != 0xff || v6[3] != 0x9b {
		return nil
	}
	// Well-known prefix 64:ff9b::/96: bytes 4-11 are the zero prefix, the
	// IPv4 is the low 32 bits.
	if v6[4] == 0 && v6[5] == 0 && v6[6] == 0 && v6[7] == 0 &&
		v6[8] == 0 && v6[9] == 0 && v6[10] == 0 && v6[11] == 0 {
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	}
	// Local-use prefix 64:ff9b:1::/48: IPv4 first half at bytes 6-7, second
	// half at bytes 9-10, straddling the "u" octet at byte 8.
	if v6[4] == 0 && v6[5] == 1 && v6[8] == 0 {
		return net.IPv4(v6[6], v6[7], v6[9], v6[10])
	}
	return nil
}

// hostnameOnly strips the port from a URL host ("host:port" → "host") and the
// brackets from an IPv6 literal ("[::1]" → "::1") so ParseIP can vet it.
func hostnameOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}
