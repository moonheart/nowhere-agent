package identity

import (
	"strings"
	"sync"
	"time"
)

// Login throttling (enterprise security baseline): a credential-stuffing or
// brute-force attempt on POST /api/auth/login is slowed by locking the
// (email, client IP) pair for a window after too many consecutive failures.
// The existing global per-IP request limiter bounds volume; this bounds a
// targeted attack on one account from one address.
//
// A SECOND dimension counts failures per EMAIL across every address: a botnet
// rotating source IPs can otherwise grind one account forever, each pair
// staying under the pair threshold. Once the email has accumulated
// loginMaxFailures failures within the window, the email itself is locked for
// loginLockout regardless of which address tries next. The lockout it grants
// an attacker is deliberate: 5 failures against a known email locks its
// password login for 15 minutes (TOTP accounts are unaffected — their failed
// second-factor attempts run on the separate totp throttler, and a correct
// password never records a throttle failure).
//
// In-memory, per gateway process — the documented multi-instance degradation:
// every instance counts its own failures, so N gateways multiply the failure
// budget of one (email, ip) pair by N (5*N guesses per window from one
// address, spread across instances). No shared state exists today. Operators
// running more than one gateway should front the auth surface with a shared
// reverse-proxy limiter or pin login to a single instance — the per-IP floor
// and the global request limiter are likewise per-instance in-memory. A
// shared (Redis/PG-backed) throttle is a future enhancement.

const (
	// loginMaxFailures is how many failed attempts within the window lock the
	// pair — and, independently, the email across all pairs. Low enough to
	// hurt a brute-forcer, high enough that a user's occasional typo never
	// trips it.
	loginMaxFailures = 5
	// loginFailWindow is how long consecutive failures must span to count
	// together (sliding).
	loginFailWindow = 15 * time.Minute
	// loginLockout is how long a locked pair (or email) stays locked.
	loginLockout = 15 * time.Minute
	// throttleMaxKeys caps how many distinct keys the throttlers track (shared
	// with OTPThrottler). prune reaps only the key it is given, so a rotating
	// attacker could otherwise grow the maps without bound; once a map crosses
	// this cap, new keys are refused after a full sweep of expired entries.
	throttleMaxKeys = 100_000
)

// LoginThrottler tracks failed login attempts per (email, ip) pair and per
// email across all pairs (see the package doc).
type LoginThrottler struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	locked   map[string]time.Time
	// email dimension: same structure, keyed by normalized email only.
	emailFailures map[string][]time.Time
	emailLocked   map[string]time.Time
}

// NewLoginThrottler builds an empty throttler.
func NewLoginThrottler() *LoginThrottler {
	return &LoginThrottler{
		failures:      map[string][]time.Time{},
		locked:        map[string]time.Time{},
		emailFailures: map[string][]time.Time{},
		emailLocked:   map[string]time.Time{},
	}
}

// key normalizes the pair: email lowercased (logins are case-sensitive at the
// store, but a brute-forcer rotating case should not dodge the counter), IP
// verbatim from the request's forwarded-headers view.
func (t *LoginThrottler) key(email, ip string) string {
	return t.emailKey(email) + "|" + ip
}

// emailKey normalizes the email dimension key (same case-folding as the pair
// key's email half).
func (t *LoginThrottler) emailKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Check reports whether a login attempt for the pair may proceed, and how long
// the caller should tell the client to wait when it may not (retry-after
// seconds). A locked pair OR a locked email is refused regardless of its
// failure history.
func (t *LoginThrottler) Check(email, ip string) (allowed bool, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	k := t.key(email, ip)
	if until, ok := t.locked[k]; ok {
		if now.Before(until) {
			return false, until.Sub(now)
		}
		// Lock expired: clear it and the failure history.
		delete(t.locked, k)
		delete(t.failures, k)
	}
	ek := t.emailKey(email)
	if until, ok := t.emailLocked[ek]; ok {
		if now.Before(until) {
			return false, until.Sub(now)
		}
		delete(t.emailLocked, ek)
		delete(t.emailFailures, ek)
	}
	t.prune(k, now)
	t.pruneEmail(ek, now)
	if len(t.failures[k]) >= loginMaxFailures || len(t.emailFailures[ek]) >= loginMaxFailures {
		return false, loginLockout
	}
	return true, 0
}

// Fail records one failed attempt for the pair AND for the email across all
// pairs. When either sliding window holds loginMaxFailures or more failures,
// that dimension is locked for loginLockout — the same boundary Check
// enforces, kept in one place.
func (t *LoginThrottler) Fail(email, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	// Each dimension is admitted and recorded independently: one map at its
	// global cap must not suppress the other dimension's counter.
	k := t.key(email, ip)
	t.prune(k, now)
	if t.admit(k, now) {
		t.failures[k] = append(t.failures[k], now)
		if len(t.failures[k]) >= loginMaxFailures {
			t.locked[k] = now.Add(loginLockout)
		}
	}
	ek := t.emailKey(email)
	t.pruneEmail(ek, now)
	if t.admitEmail(ek, now) {
		t.emailFailures[ek] = append(t.emailFailures[ek], now)
		if len(t.emailFailures[ek]) >= loginMaxFailures {
			t.emailLocked[ek] = now.Add(loginLockout)
		}
	}
}

// Success clears the pair's and the email's history (a successful login resets
// the counter, so an attacker who found the password cannot piggyback a
// pre-existing lock).
func (t *LoginThrottler) Success(email, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.key(email, ip)
	delete(t.failures, k)
	delete(t.locked, k)
	ek := t.emailKey(email)
	delete(t.emailFailures, ek)
	delete(t.emailLocked, ek)
}

// prune drops failures outside the sliding window for one pair key. It reaps
// only that key — the global bound on map growth is enforced by admit/sweepAll.
func (t *LoginThrottler) prune(k string, now time.Time) { t.pruneMap(t.failures, k, now) }

// pruneEmail is prune for the email dimension.
func (t *LoginThrottler) pruneEmail(k string, now time.Time) { t.pruneMap(t.emailFailures, k, now) }

func (t *LoginThrottler) pruneMap(m map[string][]time.Time, k string, now time.Time) {
	cutoff := now.Add(-loginFailWindow)
	kept := m[k][:0]
	for _, at := range m[k] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(m, k)
	} else {
		m[k] = kept
	}
}

// sweepAll reaps expired entries across every key of BOTH dimensions, so a
// rotating attacker cannot hold the throttler at its cap forever.
func (t *LoginThrottler) sweepAll(now time.Time) {
	for k := range t.failures {
		t.prune(k, now)
	}
	for k := range t.emailFailures {
		t.pruneEmail(k, now)
	}
	for k, until := range t.locked {
		if !now.Before(until) {
			delete(t.locked, k)
			delete(t.failures, k)
		}
	}
	for k, until := range t.emailLocked {
		if !now.Before(until) {
			delete(t.emailLocked, k)
			delete(t.emailFailures, k)
		}
	}
}

// admit reports whether a failure for k may be recorded: always, while the
// pair map is below throttleMaxKeys; at the cap, only after a full sweep has
// made room (or the key is already tracked — recording one more failure for a
// tracked key does not grow the map).
func (t *LoginThrottler) admit(k string, now time.Time) bool { return t.admitMap(t.failures, k, now) }

// admitEmail is admit for the email dimension.
func (t *LoginThrottler) admitEmail(k string, now time.Time) bool {
	return t.admitMap(t.emailFailures, k, now)
}

func (t *LoginThrottler) admitMap(m map[string][]time.Time, k string, now time.Time) bool {
	if len(m) < throttleMaxKeys {
		return true
	}
	t.sweepAll(now)
	if _, active := m[k]; active {
		return true
	}
	return len(m) < throttleMaxKeys
}
