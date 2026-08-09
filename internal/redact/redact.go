// Package redact implements PII/secret redaction for model I/O. It is the
// "desensitization" layer the platform applies at the tool-result boundary:
// emails, card numbers, IPs, bearer/basic auth tokens, provider API keys,
// private keys, and labeled secret values are detected in a tool's output and
// replaced before the result reaches the model or the durable record.
//
// The engine borrows the match-span model from LangChain's _redaction.py: every
// detector reports byte spans over the input; matches are sorted by start and
// applied non-overlapping (earliest start wins, longer wins ties), with a
// forward cursor so replacements never shift later offsets. Detectors that
// benefit from an extra check carry a verify predicate (Luhn for cards, octet
// ranges for IPs, base64 shape for basic auth) to cut false positives.
//
// Redaction is a POST-HOC visual/textual layer: it masks what a tool echoes,
// but it cannot stop a tool from actively exfiltrating a secret it already has
// (e.g. a curl that forwards an Authorization header). That boundary is the
// execution-permission gate's job; redaction and the gate are complementary.
package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Category names one class of sensitive content the redactor can detect.
type Category string

const (
	// CategoryEmail detects email addresses.
	CategoryEmail Category = "email"
	// CategoryCreditCard detects card numbers (13-19 digits, Luhn-valid).
	CategoryCreditCard Category = "credit_card"
	// CategoryIPv4 detects dotted-quad addresses with valid octets.
	CategoryIPv4 Category = "ipv4"
	// CategoryBearer detects "bearer <token>" and "Bearer <token>".
	CategoryBearer Category = "bearer"
	// CategoryBasicAuth detects "Basic <base64 creds>" with a base64-shaped token.
	CategoryBasicAuth Category = "basic_auth"
	// CategoryAPIKey detects well-known provider key prefixes (OpenAI, Anthropic,
	// Google, AWS, GitHub, Slack) and JWT-shaped tokens.
	CategoryAPIKey Category = "api_key"
	// CategoryPrivateKey detects PEM-encoded private key blocks.
	CategoryPrivateKey Category = "private_key"
	// CategorySecretValue detects "<label>: <value>" / "<label>=<value>" where the
	// label is key/secret/password/token/api_key and the value looks secret.
	CategorySecretValue Category = "secret_value"
)

// Strategy is how a detected value is replaced in the output.
type Strategy string

const (
	// StrategyRedact replaces the whole value with [REDACTED_<TYPE>].
	StrategyRedact Strategy = "redact"
	// StrategyMask replaces everything but the last four characters with ***,
	// so a reader can still recognize their own key or card.
	StrategyMask Strategy = "mask"
)

// Config controls which categories are detected and how they are replaced.
type Config struct {
	// Enabled gates redaction. False returns a nil Redactor from New.
	Enabled bool
	// Strategy is the replacement strategy; empty defaults to StrategyRedact.
	Strategy Strategy
	// Categories is a comma-separated subset of the Category constants. Empty
	// means every category is redacted.
	Categories string
}

// Match is one detected span in an input string. Start/End are byte offsets.
type Match struct {
	Start int
	End   int
	Cat   Category
}

// detector pairs a category with its regex and an optional extra check.
type detector struct {
	cat    Category
	re     *regexp.Regexp
	verify func(string) bool
}

// allDetectors is the fixed detector catalog. Detection order is irrelevant:
// Redact sorts matches by start and skips overlaps, so the earliest-starting
// span always owns a region.
var allDetectors = []detector{
	{cat: CategoryEmail, re: reEmail},
	{cat: CategoryCreditCard, re: reCreditCard, verify: luhn},
	{cat: CategoryIPv4, re: reIPv4, verify: validOctets},
	{cat: CategoryBearer, re: reBearer},
	{cat: CategoryBasicAuth, re: reBasicAuth, verify: base64ish},
	{cat: CategoryAPIKey, re: reAPIKey},
	{cat: CategoryPrivateKey, re: rePrivateKey},
	{cat: CategorySecretValue, re: reSecretValue},
}

// Redactor replaces sensitive spans in strings. It is immutable after New, so a
// single instance is safe to share across concurrent tool dispatches.
type Redactor struct {
	strategy Strategy
	cats     map[Category]bool
}

// New builds a Redactor from cfg. It returns (nil, nil) when redaction is
// disabled. An unknown strategy or category is an error — a typo'd env value
// should fail startup rather than silently redact nothing (or worse).
func New(cfg Config) (*Redactor, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyRedact
	}
	if cfg.Strategy != StrategyRedact && cfg.Strategy != StrategyMask {
		return nil, fmt.Errorf("redact: unknown strategy %q (want %q or %q)", cfg.Strategy, StrategyRedact, StrategyMask)
	}
	cats := make(map[Category]bool)
	if strings.TrimSpace(cfg.Categories) != "" {
		for _, part := range strings.Split(cfg.Categories, ",") {
			c := Category(strings.TrimSpace(part))
			if !known(c) {
				return nil, fmt.Errorf("redact: unknown category %q", c)
			}
			cats[c] = true
		}
	}
	if len(cats) == 0 {
		for _, d := range allDetectors {
			cats[d.cat] = true
		}
	}
	return &Redactor{strategy: cfg.Strategy, cats: cats}, nil
}

// Redact returns s with every detected sensitive span replaced per the
// strategy. Non-overlapping: the earliest-starting match owns its region and a
// later match inside it is skipped. When no match is found s is returned
// unchanged (and a non-empty result that survives redaction is byte-identical).
func (r *Redactor) Redact(s string) string {
	if s == "" || len(r.cats) == 0 {
		return s
	}
	var matches []Match
	for _, d := range allDetectors {
		if !r.cats[d.cat] {
			continue
		}
		for _, loc := range d.re.FindAllStringIndex(s, -1) {
			if d.verify != nil && !d.verify(s[loc[0]:loc[1]]) {
				continue
			}
			matches = append(matches, Match{Start: loc[0], End: loc[1], Cat: d.cat})
		}
	}
	if len(matches) == 0 {
		return s
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Start != matches[j].Start {
			return matches[i].Start < matches[j].Start
		}
		return matches[i].End > matches[j].End // longer span wins a tie
	})
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range matches {
		if m.Start < last {
			continue // inside a span an earlier match already covers
		}
		b.WriteString(s[last:m.Start])
		b.WriteString(r.replacement(s[m.Start:m.End], m.Cat))
		last = m.End
	}
	b.WriteString(s[last:])
	return b.String()
}

func (r *Redactor) replacement(val string, c Category) string {
	if r.strategy == StrategyMask {
		if len(val) > 4 {
			val = val[len(val)-4:]
		}
		return "***" + val
	}
	return "[REDACTED_" + strings.ToUpper(string(c)) + "]"
}

func known(c Category) bool {
	for _, d := range allDetectors {
		if d.cat == c {
			return true
		}
	}
	return false
}
