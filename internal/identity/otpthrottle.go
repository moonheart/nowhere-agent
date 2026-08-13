package identity

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// OTP throttling (anti-brute-force + anti-SMS-bombing for the phone/OTP
// routes, which are open — no bearer token exists yet):
//
//   - Verify is throttled per (phone, ip) like password login: too many wrong
//     guesses lock the pair, on top of the per-code attempt cap.
//   - Sending is capped per phone (a daily quota per number, so an attacker
//     cannot burn the deployment's SMS budget or harass one user) AND per IP
//     (a daily quota per address, so a scripted sweep across numbers cannot
//     multiply sends). The per-code 60s cooldown stays the fine-grained wall.
//
// In-memory by design, like LoginThrottler: single gateway process; multi-
// instance deployments share the effect per instance, acceptable for the
// attack class this stops.

const (
	// otpMaxVerifyFailures locks the (phone, ip) pair after this many wrong
	// codes within the window.
	otpMaxVerifyFailures = 5
	// otpVerifyWindow is how long consecutive failures span to count together.
	otpVerifyWindow = 15 * time.Minute
	// otpVerifyLockout is how long a locked pair stays locked.
	otpVerifyLockout = 15 * time.Minute
	// otpDailyPerPhone caps code sends to one number per rolling day.
	otpDailyPerPhone = 10
	// otpDailyPerIP caps code sends from one address per rolling day.
	otpDailyPerIP = 30
	// otpDay is the rolling quota window.
	otpDay = 24 * time.Hour
)

// ErrOTPSendQuota is returned when a daily send quota (per phone or per IP)
// is exhausted.
var ErrOTPSendQuota = errors.New("daily verification-code quota exceeded")

// OTPThrottler tracks verify failures and send quotas for the phone routes.
type OTPThrottler struct {
	mu        sync.Mutex
	now       func() time.Time
	verify    map[string][]time.Time
	locked    map[string]time.Time
	sentPhone map[string][]time.Time
	sentIP    map[string][]time.Time
}

// NewOTPThrottler builds an empty throttler.
func NewOTPThrottler() *OTPThrottler {
	return &OTPThrottler{
		now:       time.Now,
		verify:    map[string][]time.Time{},
		locked:    map[string]time.Time{},
		sentPhone: map[string][]time.Time{},
		sentIP:    map[string][]time.Time{},
	}
}

func (t *OTPThrottler) key(phone, ip string) string {
	return strings.TrimSpace(phone) + "|" + ip
}

// CheckVerify reports whether a verify attempt for the pair may proceed, and
// the retry-after when it may not.
func (t *OTPThrottler) CheckVerify(phone, ip string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	k := t.key(phone, ip)
	if until, ok := t.locked[k]; ok {
		if now.Before(until) {
			return false, until.Sub(now)
		}
		delete(t.locked, k)
		delete(t.verify, k)
	}
	t.prune(t.verify, k, now, otpVerifyWindow)
	if len(t.verify[k]) >= otpMaxVerifyFailures {
		return false, otpVerifyLockout
	}
	return true, 0
}

// FailVerify records one wrong code for the pair, locking it at the cap.
func (t *OTPThrottler) FailVerify(phone, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	k := t.key(phone, ip)
	t.prune(t.verify, k, now, otpVerifyWindow)
	if !t.admit(t.verify, k, now) {
		return // at the global cap: refuse to track yet another rotating key
	}
	t.verify[k] = append(t.verify[k], now)
	if len(t.verify[k]) >= otpMaxVerifyFailures {
		t.locked[k] = now.Add(otpVerifyLockout)
	}
}

// SuccessVerify clears the pair's history.
func (t *OTPThrottler) SuccessVerify(phone, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.key(phone, ip)
	delete(t.verify, k)
	delete(t.locked, k)
}

// AllowSend reports whether a code may be sent for the phone from the ip,
// enforcing the per-phone and per-IP daily quotas.
func (t *OTPThrottler) AllowSend(phone, ip string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.prune(t.sentPhone, phone, now, otpDay)
	t.prune(t.sentIP, ip, now, otpDay)
	if len(t.sentPhone[phone]) >= otpDailyPerPhone {
		return ErrOTPSendQuota
	}
	if len(t.sentIP[ip]) >= otpDailyPerIP {
		return ErrOTPSendQuota
	}
	return nil
}

// RecordSend counts one code sent for the phone from the ip.
func (t *OTPThrottler) RecordSend(phone, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.admit(t.sentPhone, phone, now) {
		t.sentPhone[phone] = append(t.sentPhone[phone], now)
	}
	if t.admit(t.sentIP, ip, now) {
		t.sentIP[ip] = append(t.sentIP[ip], now)
	}
}

// prune drops entries outside the window and reaps dead keys. It reaps only
// the given key — the global bound on map growth is enforced by admit/sweepAll.
func (t *OTPThrottler) prune(m map[string][]time.Time, k string, now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
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

// sweepAll reaps expired entries across every map, so a rotating attacker
// cannot hold the throttler at its cap forever.
func (t *OTPThrottler) sweepAll(now time.Time) {
	for k := range t.verify {
		t.prune(t.verify, k, now, otpVerifyWindow)
	}
	for k, until := range t.locked {
		if !now.Before(until) {
			delete(t.locked, k)
			delete(t.verify, k)
		}
	}
	for k := range t.sentPhone {
		t.prune(t.sentPhone, k, now, otpDay)
	}
	for k := range t.sentIP {
		t.prune(t.sentIP, k, now, otpDay)
	}
}

// admit reports whether a new entry for k may be recorded in m: always, while
// m is below throttleMaxKeys; at the cap, only after a full sweep has made
// room (or the key is already tracked — recording for a tracked key does not
// grow the map).
func (t *OTPThrottler) admit(m map[string][]time.Time, k string, now time.Time) bool {
	if len(m) < throttleMaxKeys {
		return true
	}
	t.sweepAll(now)
	if _, active := m[k]; active {
		return true
	}
	return len(m) < throttleMaxKeys
}
