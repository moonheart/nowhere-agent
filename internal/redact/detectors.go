package redact

import (
	"regexp"
	"strings"
)

// The detector regexes are written for precision over recall: every detector is
// gated by an anchor (a label like "bearer"/"basic"/"password", a provider key
// prefix, a PEM fence, an @-sign) or by a post-regex verify (Luhn, octet range,
// base64 shape), so ordinary prose and logs pass through unredacted.

var (
	// reEmail needs a full address with a dot in the domain; bare "user@host"
	// (no TLD) is left alone.
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

	// reCreditCard matches 13-19 digit runs with optional space/dash separators,
	// always ending on a digit; luhn filters the candidates. Ends-on-digit keeps
	// a trailing separator out of the span.
	reCreditCard = regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`)

	// reIPv4 matches dotted quads; validOctets rejects octets over 255.
	reIPv4 = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

	// reBearer requires the literal word "bearer" before a token-like tail, so
	// arbitrary long base64-looking strings in logs are not caught.
	reBearer = regexp.MustCompile(`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=\-]{8,}`)

	// reBasicAuth requires "basic" before a base64-shaped tail; base64ish then
	// rejects all-lowercase tails ("basic tutorial").
	reBasicAuth = regexp.MustCompile(`(?i)\bbasic[ \t]+[A-Za-z0-9+/=\-]{8,}`)

	// reAPIKey anchors on well-known provider prefixes plus JWT-shaped tokens
	// (eyJ… three dot-separated base64url parts).
	reAPIKey = regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9\-_]{20,}|sk-[A-Za-z0-9\-_]{20,}|AIza[A-Za-z0-9\-_]{30,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprsx]-[A-Za-z0-9\-]{20,}|eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,})\b`)

	// rePrivateKey spans the whole PEM block, so headers and base64 body are
	// replaced as one unit. [\s\S] is the RE2 idiom for "any byte".
	rePrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

	// reSecretValue requires a label (key/secret/password/token/…) followed by
	// a ':' or '=' separator and a value of at least 6 chars — short values
	// like "password: none" do not look secret.
	reSecretValue = regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?key|secret|password|passwd|token|auth[_-]?token|client[_-]?secret)\b\s*[:=]\s*["']?[A-Za-z0-9._\-+/=]{6,}["']?`)
)

// luhn verifies a candidate card number by the Luhn checksum, after stripping
// separators. It returns false for spans whose digit count is outside 13-19.
func luhn(s string) bool {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			b.WriteByte(c)
		}
	}
	digits := b.String()
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		v := int(digits[i] - '0')
		if double {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return sum%10 == 0
}

// validOctets rejects dotted quads with an octet over 255.
func validOctets(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			c := p[i]
			if c < '0' || c > '9' {
				return false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// base64ish distinguishes a base64 credential tail from plain prose: it must
// contain an uppercase letter, a digit, or a base64 padding/special char.
// "Basic tutorial" fails (all lowercase); "Basic dXNlcjpwYXNz" passes (has X).
func base64ish(s string) bool {
	t := strings.TrimSpace(s)
	if i := strings.IndexAny(t, " \t"); i >= 0 {
		t = t[i+1:]
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c >= 'A' && c <= 'Z' {
			return true
		}
		if c >= '0' && c <= '9' {
			return true
		}
		if c == '+' || c == '/' || c == '=' {
			return true
		}
	}
	return false
}
