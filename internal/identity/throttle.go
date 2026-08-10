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
// In-memory by design: a single gateway process. Multi-instance deployments
// share the limiter's effect only per instance — an acceptable trade for the
// class of attack this stops (a remote attacker cannot use N instances to
// multiply attempts against the same pair unless they control the pairing).

const (
	// loginMaxFailures is how many failed attempts within the window lock the
	// pair. Low enough to hurt a brute-forcer, high enough that a user's
	// occasional typo never trips it.
	loginMaxFailures = 5
	// loginFailWindow is how long consecutive failures must span to count
	// together (sliding).
	loginFailWindow = 15 * time.Minute
	// loginLockout is how long a locked pair stays locked.
	loginLockout = 15 * time.Minute
)

// LoginThrottler tracks failed login attempts per (email, ip) pair.
type LoginThrottler struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	locked   map[string]time.Time
}

// NewLoginThrottler builds an empty throttler.
func NewLoginThrottler() *LoginThrottler {
	return &LoginThrottler{
		failures: map[string][]time.Time{},
		locked:   map[string]time.Time{},
	}
}

// key normalizes the pair: email lowercased (logins are case-sensitive at the
// store, but a brute-forcer rotating case should not dodge the counter), IP
// verbatim from the request's forwarded-headers view.
func (t *LoginThrottler) key(email, ip string) string {
	return strings.ToLower(strings.TrimSpace(email)) + "|" + ip
}

// Check reports whether a login attempt for the pair may proceed, and how long
// the caller should tell the client to wait when it may not (retry-after
// seconds). A locked pair is refused regardless of its failure history.
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
	t.prune(k, now)
	if len(t.failures[k]) >= loginMaxFailures {
		return false, loginLockout
	}
	return true, 0
}

// Fail records one failed attempt for the pair. When the sliding window now
// holds loginMaxFailures or more failures, the pair is locked for
// loginLockout — the same boundary Check enforces, kept in one place.
func (t *LoginThrottler) Fail(email, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	k := t.key(email, ip)
	t.prune(k, now)
	t.failures[k] = append(t.failures[k], now)
	if len(t.failures[k]) >= loginMaxFailures {
		t.locked[k] = now.Add(loginLockout)
	}
}

// Success clears the pair's history (a successful login resets the counter, so
// an attacker who found the password cannot piggyback a pre-existing lock).
func (t *LoginThrottler) Success(email, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.key(email, ip)
	delete(t.failures, k)
	delete(t.locked, k)
}

// prune drops failures outside the sliding window and reaps dead pairs, so a
// busy attacker cannot grow the map without bound.
func (t *LoginThrottler) prune(k string, now time.Time) {
	cutoff := now.Add(-loginFailWindow)
	kept := t.failures[k][:0]
	for _, at := range t.failures[k] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(t.failures, k)
	} else {
		t.failures[k] = kept
	}
}
