package redact

import (
	"strings"
	"testing"
)

// must builds an enabled Redactor or fails the test.
func must(t *testing.T, cfg Config) *Redactor {
	t.Helper()
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	if r == nil {
		t.Fatal("redact.New returned nil for an enabled config")
	}
	return r
}

// TestRedactDisabled: New returns a nil Redactor when not enabled.
func TestRedactDisabled(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatalf("New(disabled) errored: %v", err)
	}
	if r != nil {
		t.Error("New(disabled) must return nil")
	}
}

// TestRedactInvalidConfig: an unknown strategy or category is a config error,
// never a silent no-op.
func TestRedactInvalidConfig(t *testing.T) {
	if _, err := New(Config{Enabled: true, Strategy: "obfuscate"}); err == nil {
		t.Error("unknown strategy must error")
	}
	if _, err := New(Config{Enabled: true, Categories: "telephone"}); err == nil {
		t.Error("unknown category must error")
	}
}

// TestRedactEmail: an address is replaced and the surrounding text survives.
func TestRedactEmail(t *testing.T) {
	r := must(t, Config{Enabled: true})
	got := r.Redact("contact me at alice@example.com please")
	if strings.Contains(got, "alice@example.com") {
		t.Errorf("email leaked through: %q", got)
	}
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Errorf("email placeholder missing: %q", got)
	}
	if !strings.HasPrefix(got, "contact me at ") || !strings.HasSuffix(got, " please") {
		t.Errorf("surrounding text not preserved: %q", got)
	}
}

// TestRedactCreditCardLuhn: a Luhn-valid card is redacted, an invalid one is not.
func TestRedactCreditCardLuhn(t *testing.T) {
	r := must(t, Config{Enabled: true})

	valid := "card: 4111 1111 1111 1111"
	if got := r.Redact(valid); strings.Contains(got, "4111") {
		t.Errorf("valid card not redacted: %q", got)
	}

	invalid := "card: 4111 1111 1111 1112" // same digits, checksum off
	if got := r.Redact(invalid); !strings.Contains(got, "4111 1111 1111 1112") {
		t.Errorf("invalid card must pass through unchanged: %q", got)
	}

	dashed := "card: 4242-4242-4242-4242" // Luhn-valid, dash separated
	if got := r.Redact(dashed); strings.Contains(got, "4242") {
		t.Errorf("dashed valid card not redacted: %q", got)
	}
}

// TestRedactIPv4: a valid dotted quad is redacted; an out-of-range octet passes.
func TestRedactIPv4(t *testing.T) {
	r := must(t, Config{Enabled: true})
	if got := r.Redact("server at 192.168.0.1:8080"); strings.Contains(got, "192.168.0.1") {
		t.Errorf("IPv4 not redacted: %q", got)
	}
	bad := "server at 999.1.1.1 is up"
	if got := r.Redact(bad); !strings.Contains(got, "999.1.1.1") {
		t.Errorf("octet-over-255 must pass through unchanged: %q", got)
	}
}

// TestRedactBearer: label-anchored bearer tokens are redacted; prose is not.
func TestRedactBearer(t *testing.T) {
	r := must(t, Config{Enabled: true})
	auth := "Authorization: Bearer abcdefghijklmnopqrstuvwxyz"
	if got := r.Redact(auth); strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("bearer token leaked through: %q", got)
	}
	if got := r.Redact(auth); !strings.Contains(got, "[REDACTED_BEARER]") {
		t.Errorf("bearer placeholder missing: %q", got)
	}
	if got := r.Redact("bearer bonds are a safe investment"); got != "bearer bonds are a safe investment" {
		t.Errorf("prose containing 'bearer' must pass through: %q", got)
	}
}

// TestRedactBasicAuth: base64-shaped basic credentials are redacted; plain
// prose after "basic" is not.
func TestRedactBasicAuth(t *testing.T) {
	r := must(t, Config{Enabled: true})
	if got := r.Redact("Authorization: Basic dXNlcjpwYXNz"); strings.Contains(got, "dXNlcjpwYXNz") {
		t.Errorf("basic auth token leaked through: %q", got)
	}
	if got := r.Redact("start with a basic tutorial first"); got != "start with a basic tutorial first" {
		t.Errorf("'basic tutorial' must pass through: %q", got)
	}
}

// TestRedactAPIKeys: each well-known provider key prefix is redacted.
func TestRedactAPIKeys(t *testing.T) {
	r := must(t, Config{Enabled: true})
	keys := []string{
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz123456789",
		"sk-proj-abcdefghijklmnopqrstuvwxyz1234567890123",
		"AIzaSyBKP6TSTaadwvWqGXh1m3FhRlU7Wt0kXcM",
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ012345",
		"xoxb-1234567890123-4567890123456-abcdefghijklm",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	}
	for _, k := range keys {
		if got := r.Redact("key: " + k); strings.Contains(got, k[4:]) && strings.Contains(got, "key: ") {
			t.Errorf("api key leaked through: %q", got)
		}
		if !strings.Contains(r.Redact("key: "+k), "[REDACTED_API_KEY]") {
			t.Errorf("api key placeholder missing for %q", k)
		}
	}
}

// TestRedactPrivateKey: a PEM block is replaced as one unit.
func TestRedactPrivateKey(t *testing.T) {
	r := must(t, Config{Enabled: true})
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----"
	got := r.Redact("here is the key:\n" + pem)
	if strings.Contains(got, "PRIVATE KEY-----") || strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Errorf("private key block leaked through: %q", got)
	}
	if !strings.Contains(got, "[REDACTED_PRIVATE_KEY]") {
		t.Errorf("private key placeholder missing: %q", got)
	}
}

// TestRedactSecretValue: labeled values are redacted; short/non-secret values
// like "password: none" pass through.
func TestRedactSecretValue(t *testing.T) {
	r := must(t, Config{Enabled: true})
	if got := r.Redact("db password: hunter2"); !strings.Contains(got, "[REDACTED_SECRET_VALUE]") {
		t.Errorf("labeled secret not redacted: %q", got)
	}
	if got := r.Redact("api_key=abcdef123456"); !strings.Contains(got, "[REDACTED_SECRET_VALUE]") {
		t.Errorf("api_key assignment not redacted: %q", got)
	}
	if got := r.Redact("password: none"); got != "password: none" {
		t.Errorf("'password: none' must pass through: %q", got)
	}
}

// TestRedactMask: mask keeps the last four characters, bounded.
func TestRedactMask(t *testing.T) {
	r := must(t, Config{Enabled: true, Strategy: StrategyMask})
	got := r.Redact("card: 4111 1111 1111 1111")
	if !strings.HasSuffix(got, "***1111") {
		t.Errorf("mask should keep last 4: %q", got)
	}
	if strings.Contains(got, "4111 1111") {
		t.Errorf("mask leaked the middle of the card: %q", got)
	}
}

// TestRedactCategoriesFilter: narrowing to one category leaves the others alone.
func TestRedactCategoriesFilter(t *testing.T) {
	r := must(t, Config{Enabled: true, Categories: "email"})
	in := "alice@example.com sk-ant-abcdefghijklmnopqrstuvwxyz123456789"
	got := r.Redact(in)
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Errorf("email not redacted: %q", got)
	}
	if !strings.Contains(got, "sk-ant-abcdefghijklmnopqrstuvwxyz123456789") {
		t.Errorf("api key must pass through when filtered out: %q", got)
	}
}

// TestRedactOverlap: an outer span (bearer+token) wins over the inner api_key
// span inside it, so the result has exactly one placeholder.
func TestRedactOverlap(t *testing.T) {
	r := must(t, Config{Enabled: true})
	in := "Authorization: Bearer sk-ant-abcdefghijklmnopqrstuvwxyz123456789"
	got := r.Redact(in)
	if strings.Contains(got, "sk-ant-") {
		t.Errorf("nested api key leaked through: %q", got)
	}
	if n := strings.Count(got, "REDACTED_"); n != 1 {
		t.Errorf("overlapping spans should collapse to one placeholder, got %d in %q", n, got)
	}
}

// TestRedactNoMatch: content with nothing sensitive passes through byte-exact.
func TestRedactNoMatch(t *testing.T) {
	r := must(t, Config{Enabled: true})
	in := "the build passed in 42.1s, all 137 tests green"
	if got := r.Redact(in); got != in {
		t.Errorf("clean text must pass through unchanged: %q", got)
	}
}
