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
// Expired entries are evicted lazily on read and overwritten on the next
// put; the key space (users, teams, providers, model names) is bounded by
// the registry's size, so no background sweep is needed. Concurrent loads on
// a miss are allowed to duplicate (last write wins) — a resolution is cheap
// enough that singleflight machinery is not worth the lock hold.
type ttlCache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]ttlCacheEntry[V]
}

type ttlCacheEntry[V any] struct {
	value V
	err   error
	until time.Time
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, entries: map[string]ttlCacheEntry[V]{}}
}

// get returns the cached entry when present and fresh.
func (c *ttlCache[V]) get(key string) (ttlCacheEntry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.until) {
		return ttlCacheEntry[V]{}, false
	}
	return e, true
}

// put caches the value (or the deterministic-negative error) for the TTL.
func (c *ttlCache[V]) put(key string, value V, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = ttlCacheEntry[V]{value: value, err: err, until: time.Now().Add(c.ttl)}
}
