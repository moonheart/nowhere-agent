// SSRF guard for outbound webhook delivery. Webhook URLs come from
// configurable, user-written sources (scheduled-task webhook_url, inbound
// webhook notify_url, the global WEBHOOK_URL), so a misconfigured or hostile
// URL can otherwise make the platform's server POST into its own private
// network — the classic server-side-request-forgery. The guard runs at
// delivery time, on every attempt, right before any connection is made:
//
//   - ValidateURL rejects anything that is not an absolute http(s) URL.
//   - CheckURL/ResolveURL additionally resolves the host and rejects the
//     target when any resolved address is loopback/private/link-local/
//     multicast — unless the host or the address is explicitly allowlisted
//     (the escape hatch for legitimately internal notification targets: an
//     intranet IM gateway, a workflow engine on a private subnet, …).
//   - ResolveURL returns the vetted addresses, which the delivery client pins
//     into the request context; Guard.DialContext then dials exactly those
//     addresses, so a host cannot rebind to a different (private) address
//     between the check and the connection.
//   - CheckRedirect re-validates every redirect hop with the same rules and
//     re-pins it, so a public URL cannot smuggle the request to a private
//     address via a 302.
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

	"nowhere-agent/internal/netutil"
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
	_, err := g.ResolveURL(ctx, raw)
	return err
}

// ResolveURL is CheckURL plus the vetted addresses: it validates raw, resolves
// the host, and refuses the target when any resolved address is private
// (unless allowlisted), returning the addresses a delivery to raw may connect
// to. The caller pins them into the request context so Guard.DialContext can
// connect to exactly those addresses — closing the check-to-dial window. An
// allowlisted hostname returns (nil, nil): the operator trusted the name, so
// its addresses are not vetted and the delivery dials them freely by design.
func (g *Guard) ResolveURL(ctx context.Context, raw string) ([]net.IP, error) {
	if err := g.ValidateURL(raw); err != nil {
		return nil, err
	}
	u, _ := url.Parse(raw)
	host := hostnameOnly(u.Host)
	if ip := net.ParseIP(host); ip != nil {
		// Literal IP: already decided by ValidateURL via allowedIP.
		if !g.allowedIP(ip, "") {
			return nil, ErrBlocked
		}
		return []net.IP{ip}, nil
	}
	// Allowlisted hostname bypasses resolution entirely (an internal name may
	// not resolve on the platform's public DNS, which is exactly why the
	// operator allowlisted it).
	if _, ok := g.allowHosts[strings.ToLower(host)]; ok {
		return nil, nil
	}
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		// A resolution failure is refused, not passed through: the target
		// cannot be vetted, and failing closed is the safe side.
		return nil, fmt.Errorf("%w: resolve %q: %v", ErrBlocked, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %q resolved to no addresses", ErrBlocked, host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if !g.allowedIP(a.IP, host) {
			return nil, fmt.Errorf("%w: %q resolves to %s", ErrBlocked, host, a.IP)
		}
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// pinnedIPKey is the request-context key for the vetted address set of the
// delivery that context belongs to (see withPinned).
type pinnedIPKey struct{}

// pinnedDial is the pin ResolveURL stores per delivery: the hostname whose
// addresses were vetted and the vetted address set DialContext must connect
// to. The host is kept so a dial for a different host (an HTTP proxy from the
// environment, say) is not wrongly pinned.
type pinnedDial struct {
	host string
	ips  []net.IP
}

func withPinned(ctx context.Context, p pinnedDial) context.Context {
	return context.WithValue(ctx, pinnedIPKey{}, p)
}

// pinRequest validates raw with ResolveURL and, when addresses were vetted
// (an allowlisted hostname bypasses vetting and is not pinned), pins them into
// req's context so the transport dials exactly those addresses. Installed as
// the http client's CheckRedirect it also re-pins every redirect hop.
func (g *Guard) pinRequest(req *http.Request, raw string) error {
	ips, err := g.ResolveURL(req.Context(), raw)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return nil
	}
	u, _ := url.Parse(raw)
	*req = *req.WithContext(withPinned(req.Context(), pinnedDial{
		host: strings.ToLower(u.Hostname()),
		ips:  ips,
	}))
	return nil
}

// CheckRedirect re-validates every redirect hop (installed as the http
// client's CheckRedirect) and re-pins the hop's vetted addresses into the hop
// request, so a 302 cannot smuggle the request to a private address — neither
// the validation nor the dial can be skipped by the redirect.
func (g *Guard) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("webhook: too many redirects")
	}
	return g.pinRequest(req, req.URL.String())
}

// DialContext is the http.Transport dial hook that closes the check-to-dial
// race: when the request context carries a pin (ResolveURL/pinRequest), it
// connects to exactly those vetted addresses — never re-resolving the host —
// so a host that rebinds between the SSRF check and the connection cannot
// redirect the dial to a private address. The URL's host stays the request
// host (and TLS SNI); only the destination IP is pinned. A dial whose address
// does not match the pinned host (e.g. an HTTP proxy from the environment) or
// a request without a pin (an allowlisted hostname, by design) falls back to
// a plain dial — a proxy is transport plumbing, not a vetted target, and an
// allowlisted name is the operator's explicit trust.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{}
	p, ok := ctx.Value(pinnedIPKey{}).(pinnedDial)
	if !ok {
		return d.DialContext(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !strings.EqualFold(host, p.host) {
		return d.DialContext(ctx, network, addr)
	}
	if len(p.ips) == 0 {
		return nil, fmt.Errorf("webhook: no pinned addresses for %q", p.host)
	}
	var lastErr error
	for _, ip := range p.ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	// No fallback to the hostname: only the vetted addresses may be dialed,
	// or a rebinding host could slip the guard after all.
	return nil, fmt.Errorf("webhook: dial pinned addresses for %q: %w", p.host, lastErr)
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
	// private target smuggled as [64:ff9b::10.0.0.1], [2002:a00:1::],
	// [2001:db8::5efe:10.0.0.1] or [::a00:1] evades a 10.0.0.0/8 allowlist
	// that only sees the raw IPv6. Every possible reading of an ambiguous
	// NAT64 address is checked (see netutil.EmbeddedIPv4s).
	if v4s := netutil.EmbeddedIPv4s(ip); len(v4s) > 0 {
		for _, v4 := range v4s {
			for _, n := range g.allowNets {
				if n.Contains(v4) {
					return true
				}
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
	// Normalize to the 4-byte form: net.IPv4(a,b,c,d) returns a 16-byte
	// slice (IPv4-mapped), so index checks like ip[0] must not read the
	// mapping prefix. public6 feeds embedded readings straight in here.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified(), ip[0] == 0:
		return false
	}
	// 0.0.0.0/8 is "this network" (RFC 6890): the kernel routes the whole
	// range to the local host (route table local → lo), so 0.0.0.x dials
	// reach a local service just like 127.0.0.x — IsUnspecified only covers
	// 0.0.0.0 itself. The ip[0] == 0 case above blocks the full /8.
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
	// Translation schemes embed an IPv4 the translator reaches on the
	// caller's behalf: vet THAT address, or a private target smuggled as
	// [64:ff9b::10.0.0.1], [2002:a00:1::], [2001:db8::5efe:10.0.0.1] or
	// [::a00:1] would slip past the IPv6 checks (IsPrivate etc. do not
	// cover these encodings). A NAT64 local-use address has several
	// possible readings (netutil.EmbeddedIPv4s): it is refused when ANY
	// reading is private, since the actual translator parameter is not
	// observable from the address alone.
	if v4s := netutil.EmbeddedIPv4s(ip); len(v4s) > 0 {
		for _, v4 := range v4s {
			if !public4(v4) {
				return false
			}
		}
		return true
	}
	return true
}

// hostnameOnly strips the port from a URL host ("host:port" → "host") and the
// brackets from an IPv6 literal ("[::1]" → "::1") so ParseIP can vet it.
func hostnameOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}
