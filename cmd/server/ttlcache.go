package main

import (
	"sync"
	"time"
)

// stringTTLCache is a tiny process-local TTL cache for string values keyed by
// string. Built for the permission-mode lookup: the tool gate consults the
// session's permission_mode twice per gated call (interaction gate, then
// execution gate) and each consultation is a PG read via SessionStateKV, so a
// batch of 10 ask-risk calls cost 20 queries. A short TTL collapses the
// double read to one per window per session — and, as a side benefit, makes
// the two gate consultations within one call agree. The client's "allow all"
// toggle stays effectively live: it only ever applied to the NEXT tool call,
// and now that next call may read a value up to the TTL old.
//
// Load errors are never cached (a store hiccup must not stick); concurrent
// loads on a miss may duplicate (last write wins) — the read is cheap enough
// that singleflight machinery is not worth the lock hold.
type stringTTLCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]stringTTLCacheEntry
}

type stringTTLCacheEntry struct {
	value string
	until time.Time
}

func newStringTTLCache(ttl time.Duration) *stringTTLCache {
	return &stringTTLCache{ttl: ttl, entries: map[string]stringTTLCacheEntry{}}
}

// getOrLoad returns the cached value when fresh, otherwise loads and caches
// it. A load error propagates without caching.
func (c *stringTTLCache) getOrLoad(key string, load func() (string, error)) (string, error) {
	c.mu.Lock()
	e, ok := c.entries[key]
	fresh := ok && time.Now().Before(e.until)
	c.mu.Unlock()
	if fresh {
		return e.value, nil
	}
	v, err := load()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.entries[key] = stringTTLCacheEntry{value: v, until: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return v, nil
}
