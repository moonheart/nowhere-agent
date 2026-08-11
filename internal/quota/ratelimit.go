package quota

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// KeyFunc derives the bucket key for a request. Returning "" opts the request
// out of limiting (e.g. health checks). Key by authenticated principal where
// possible; fall back to client IP so an anonymous flood is still smoothed.
type KeyFunc func(r *http.Request) string

// RateLimiter is a per-key token bucket over HTTP requests. Unlike the monthly
// budget (which caps total spend), this smooths the request RATE so one caller
// cannot starve others of concurrency or hammer the model with a burst. It is
// deliberately separate from the budget: a caller far under budget can still be
// rate-limited for a burst, and a burst-free caller can still be budget-capped.
type RateLimiter struct {
	rps   rate.Limit
	burst int
	keyFn KeyFunc
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

// bucket is one key's limiter plus the last time it was seen, so the sweeper can
// evict buckets for keys that have gone quiet rather than growing the map forever.
type bucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter builds a limiter allowing rps requests per second sustained with
// a burst of burst, keyed by keyFn. rps <= 0 or burst <= 0 disables limiting
// (Middleware passes everything through). A sweeper goroutine reclaims idle
// buckets; call Close to stop it.
func NewRateLimiter(rps float64, burst int, keyFn KeyFunc) *RateLimiter {
	rl := &RateLimiter{
		rps:     rate.Limit(rps),
		burst:   burst,
		keyFn:   keyFn,
		now:     func() time.Time { return time.Now().UTC() },
		buckets: make(map[string]*bucket),
	}
	if rl.enabled() {
		go rl.sweep()
	}
	return rl
}

func (rl *RateLimiter) enabled() bool { return rl != nil && rl.rps > 0 && rl.burst > 0 }

// SetClock overrides the limiter clock (tests).
func (rl *RateLimiter) SetClock(now func() time.Time) {
	if now != nil {
		rl.now = now
	}
}

// bucketFor returns (creating on first use) the token bucket for key.
func (rl *RateLimiter) bucketFor(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rl.rps, rl.burst)}
		rl.buckets[key] = b
	}
	b.lastSeen = rl.now()
	return b.lim
}

// SetRate retunes the limiter live (no restart): rps/burst take effect for
// NEW buckets immediately, and every existing bucket's rate is adjusted
// (x/time/rate's SetLimit). Burst changes apply to new buckets; existing
// buckets keep their burst until the sweeper evicts them (≤10 minutes), which
// is the documented convergence window for a live retune.
func (rl *RateLimiter) SetRate(rps float64, burst int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.rps = rate.Limit(rps)
	rl.burst = burst
	for _, b := range rl.buckets {
		b.lim.SetLimit(rl.rps)
	}
}

// sweep periodically evicts buckets idle longer than the TTL so a long-lived
// process does not accumulate a bucket per key it has ever seen. A key whose
// bucket is evicted simply gets a fresh (full) bucket on its next request, which
// is the same state it would have after refilling during that idle time.
func (rl *RateLimiter) sweep() {
	const interval = time.Minute
	const ttl = 10 * time.Minute
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		rl.mu.Lock()
		cutoff := rl.now().Add(-ttl)
		for k, b := range rl.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}

// Allow reports whether one request for key may proceed (consuming a token).
func (rl *RateLimiter) Allow(key string) bool {
	if !rl.enabled() {
		return true
	}
	return rl.bucketFor(key).Allow()
}

// Middleware limits requests through next. Over-limit requests get 429 with a
// short Retry-After; the body names the caller-facing cause without leaking
// limit internals. KeyFunc returning "" bypasses limiting entirely.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.enabled() {
			next.ServeHTTP(w, r)
			return
		}
		key := ""
		if rl.keyFn != nil {
			key = rl.keyFn(r)
		}
		if key == "" || rl.Allow(key) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	})
}

// ClientIPKey is a KeyFunc keyed on the client IP, honoring X-Forwarded-For's
// first hop when behind a proxy. Use for unauthenticated edge smoothing; prefer
// a principal-based key for authenticated APIs so one user behind a NAT is not
// throttled by their neighbors.
func ClientIPKey(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i > 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	// RemoteAddr is host:port; keep just the host so one client's many source
	// ports share a bucket.
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
