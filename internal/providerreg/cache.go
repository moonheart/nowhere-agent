package providerreg

import (
	"sync"
	"time"
)

// ttlCache is a tiny in-process TTL cache over resolution results. Resolution
// runs on the request path — one chat submission resolves the caller's
// target, the tool binder resolves again for view_image, the model picker
// lists the provider's models — each a handful of PG round trips plus a key
// decryption, over data that changes only when an operator edits the
// registry. A TTL of a few seconds collapses that to one resolution per
// window while keeping console edits effectively live (the platform's own
// settings snapshot converges on a 30s loop, so a multi-second lag here is
// well inside the existing convergence budget).
//
// Deterministic negatives (ErrNoProvider, ErrUnknownModel) are cached
// alongside successes — a request that cannot resolve will fail closed again
// within the window without re-paying the lookup. Infrastructure errors (DB
// hiccups) are never cached: a transient failure must not stick.
//
// The entry count is hard-capped (maxEntries): the key space is NOT bounded
// by the registry's size — user keys ("u:<id>") grow monotonically with the
// user base, so an unbounded map would be a slow memory leak on a
// multi-tenant deployment. A put at capacity first evicts every expired
// entry, then the entry closest to expiring; evicted keys simply re-resolve
// on their next request, so the cap degrades the hit rate, never
// correctness. Expired entries are also dropped lazily on read miss.
// Concurrent loads on a miss are allowed to duplicate (last write wins) — a
// resolution is cheap enough that singleflight machinery is not worth the
// lock hold.
type ttlCache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]ttlCacheEntry[V]
}

type ttlCacheEntry[V any] struct {
	value V
	err   error
	until time.Time
}

// defaultMaxEntries bounds one cache's live key set. With a TTL of seconds,
// only keys active inside one window occupy entries at all; 4096 covers a
// large tenant population with headroom, at ~hundreds of bytes per entry.
const defaultMaxEntries = 4096

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, max: defaultMaxEntries, entries: map[string]ttlCacheEntry[V]{}}
}

// get returns the cached entry when present and fresh; an expired hit is
// evicted so a stale key cannot occupy capacity forever.
func (c *ttlCache[V]) get(key string) (ttlCacheEntry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return ttlCacheEntry[V]{}, false
	}
	if time.Now().After(e.until) {
		delete(c.entries, key)
		return ttlCacheEntry[V]{}, false
	}
	return e, true
}

// put caches the value (or the deterministic-negative error) for the TTL,
// bounding the map at c.max entries (evictExpiredThenOldest).
func (c *ttlCache[V]) put(key string, value V, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok && len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[key] = ttlCacheEntry[V]{value: value, err: err, until: time.Now().Add(c.ttl)}
}

// evictLocked drops every expired entry; when none are expired it drops the
// single entry closest to expiring. Callers hold the lock.
func (c *ttlCache[V]) evictLocked() {
	now := time.Now()
	var oldestKey string
	var oldestUntil time.Time
	for k, e := range c.entries {
		if now.After(e.until) {
			delete(c.entries, k)
			continue
		}
		if oldestKey == "" || e.until.Before(oldestUntil) {
			oldestKey, oldestUntil = k, e.until
		}
	}
	if len(c.entries) >= c.max && oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
