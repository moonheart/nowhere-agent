package providerreg

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// countingStore embeds the Store interface (nil — unimplemented methods panic,
// which is the point: a cached call path must not consult the store at all)
// and counts the reads resolution makes.
type countingStore struct {
	Store

	mu sync.Mutex

	userTeam       map[string]string
	assignments    map[string]TeamAssignment
	providers      map[string]Provider
	platform       Provider
	platformErr    error
	models         map[string][]Model
	enabledModelOK map[string]bool

	userTeamCalls    int
	assignmentCalls  int
	providerCalls    int
	platformCalls    int
	listModelsCalls  int
	enabledModelCall int
}

func newCountingStore() *countingStore {
	return &countingStore{
		userTeam:       map[string]string{},
		assignments:    map[string]TeamAssignment{},
		providers:      map[string]Provider{},
		models:         map[string][]Model{},
		enabledModelOK: map[string]bool{},
		platform: Provider{
			ID: "p1", Scope: ScopeSystem, Name: "plat", Vendor: VendorOpenAI, RawKey: "sk-plat", Enabled: true,
		},
	}
}

func (s *countingStore) count(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

func (s *countingStore) UserTeam(_ context.Context, userID string) (string, error) {
	s.count(func() { s.userTeamCalls++ })
	return s.userTeam[userID], nil
}

func (s *countingStore) GetTeamAssignment(_ context.Context, teamID string) (TeamAssignment, error) {
	s.count(func() { s.assignmentCalls++ })
	a, ok := s.assignments[teamID]
	if !ok {
		return TeamAssignment{}, ErrNotFound
	}
	return a, nil
}

func (s *countingStore) GetProvider(_ context.Context, id string) (Provider, error) {
	s.count(func() { s.providerCalls++ })
	p, ok := s.providers[id]
	if !ok {
		return Provider{}, ErrNotFound
	}
	return p, nil
}

func (s *countingStore) PlatformDefault(_ context.Context) (Provider, error) {
	s.count(func() { s.platformCalls++ })
	return s.platform, s.platformErr
}

func (s *countingStore) ListModels(_ context.Context, providerID string) ([]Model, error) {
	s.count(func() { s.listModelsCalls++ })
	return s.models[providerID], nil
}

func (s *countingStore) EnabledModel(_ context.Context, providerID, name string) (Model, error) {
	s.count(func() { s.enabledModelCall++ })
	if s.enabledModelOK[providerID+"\x00"+name] {
		return Model{ProviderID: providerID, Name: name, Enabled: true}, nil
	}
	return Model{}, ErrNotFound
}

func (s *countingStore) totals() (userTeam, platform, listModels, enabledModel int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userTeamCalls, s.platformCalls, s.listModelsCalls, s.enabledModelCall
}

func seedCountingDefaults(s *countingStore) {
	s.models["p1"] = []Model{{ID: "m1", ProviderID: "p1", Name: "gpt-x", Enabled: true, IsDefault: true, Vision: true}}
	s.enabledModelOK["p1\x00gpt-x"] = true
}

// TestResolverCacheCollapsesRepeatedReads: with the cache on, repeated
// resolutions and model lookups within the TTL hit the store once, not once
// per call — the request path's chat-resolve → vision-check → model-list
// sequence costs one store round in total.
func TestResolverCacheCollapsesRepeatedReads(t *testing.T) {
	s := newCountingStore()
	seedCountingDefaults(s)
	r := NewResolver(s).WithCacheTTL(time.Minute)
	ctx := context.Background()

	for range 3 {
		tg, err := r.Resolve(ctx, "u1")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if tg.ProviderID != "p1" || tg.Model != "gpt-x" {
			t.Fatalf("target = %+v", tg)
		}
		if _, ok := r.VisionModel(ctx, tg); !ok {
			t.Fatal("vision model should resolve")
		}
		if _, err := r.EnabledModels(ctx, tg); err != nil {
			t.Fatalf("enabled models: %v", err)
		}
		if _, err := r.ResolveModel(ctx, tg, "gpt-x"); err != nil {
			t.Fatalf("resolve model: %v", err)
		}
	}
	userTeam, platform, listModels, enabledModel := s.totals()
	if userTeam != 1 || platform != 1 || listModels != 1 || enabledModel != 1 {
		t.Errorf("store reads = (userTeam %d, platform %d, listModels %d, enabledModel %d), want all 1",
			userTeam, platform, listModels, enabledModel)
	}
}

// TestResolverCacheExpires: entries older than the TTL are re-read, so a
// registry edit reaches the resolver within one window.
func TestResolverCacheExpires(t *testing.T) {
	s := newCountingStore()
	seedCountingDefaults(s)
	r := NewResolver(s).WithCacheTTL(20 * time.Millisecond)
	ctx := context.Background()

	if _, err := r.Resolve(ctx, "u1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Change the platform default's model set mid-test: without expiry the
	// resolver would never see it.
	s.models["p1"] = []Model{{ID: "m2", ProviderID: "p1", Name: "gpt-y", Enabled: true, IsDefault: true}}
	time.Sleep(40 * time.Millisecond)
	tg, err := r.Resolve(ctx, "u1")
	if err != nil {
		t.Fatalf("resolve after expiry: %v", err)
	}
	if tg.Model != "gpt-y" {
		t.Errorf("model = %q, want the post-edit gpt-y after the TTL expired", tg.Model)
	}
}

// TestResolverCacheNegative: a deterministic negative (no provider) is cached
// too — a request that cannot resolve fails closed again without re-paying
// the lookup — while an infrastructure error never sticks.
func TestResolverCacheNegative(t *testing.T) {
	s := newCountingStore() // no models seeded: the platform default is unservable
	r := NewResolver(s).WithCacheTTL(20 * time.Millisecond)
	ctx := context.Background()

	for range 2 {
		if _, err := r.Resolve(ctx, "u1"); !errors.Is(err, ErrNoProvider) {
			t.Fatalf("resolve without models: %v, want ErrNoProvider", err)
		}
	}
	if _, platform, listModels, _ := s.totals(); platform != 1 || listModels != 1 {
		t.Errorf("negative resolution store reads = platform %d listModels %d, want 1 each", platform, listModels)
	}

	// A store hiccup must NOT be cached: the next call retries the store.
	s.platformErr = errors.New("db down")
	if _, err := r.Resolve(context.Background(), "u2"); err == nil || errors.Is(err, ErrNoProvider) {
		t.Fatalf("hiccup resolve: %v, want the raw store error", err)
	}
	s.platformErr = nil
	seedCountingDefaults(s)
	// Let the seeded-in empty model list's TTL lapse (the edit-convergence lag
	// the cache trades for) before expecting success.
	time.Sleep(40 * time.Millisecond)
	if _, err := r.Resolve(ctx, "u2"); err != nil {
		t.Fatalf("resolve after hiccup: %v, want success (the hiccup must not stick)", err)
	}
	_, platformReads, _, _ := s.totals()
	if platformReads != 3 {
		t.Errorf("platform reads = %d, want 3 (1 cached negative + 1 hiccup + 1 retry)", platformReads)
	}
}

// TestTTLCacheCapacityBoundsKeySpace: user keys grow with the user base, so
// the cache must stay at its capacity no matter how many distinct keys are
// put — evicted keys simply re-resolve on their next request.
func TestTTLCacheCapacityBoundsKeySpace(t *testing.T) {
	c := newTTLCache[struct{}](time.Minute)
	c.max = 8
	for i := range 100 {
		c.put("u:"+string(rune(i)), struct{}{}, nil)
	}
	if got := len(c.entries); got > c.max {
		t.Fatalf("entries = %d, want <= max %d (the map must not grow with distinct keys)", got, c.max)
	}
}

// TestTTLCacheEvictsExpiredFirst: at capacity, expired entries are reclaimed
// before any live entry is evicted.
func TestTTLCacheEvictsExpiredFirst(t *testing.T) {
	c := newTTLCache[struct{}](20 * time.Millisecond)
	c.max = 4
	c.put("stale-1", struct{}{}, nil)
	c.put("stale-2", struct{}{}, nil)
	time.Sleep(40 * time.Millisecond)
	c.put("live-1", struct{}{}, nil)
	c.put("live-2", struct{}{}, nil)
	c.put("live-3", struct{}{}, nil) // capacity reached: the two stale entries go
	if _, ok := c.entries["stale-1"]; ok {
		t.Error("expired entry stale-1 should have been evicted at capacity")
	}
	if _, ok := c.entries["stale-2"]; ok {
		t.Error("expired entry stale-2 should have been evicted at capacity")
	}
	for _, k := range []string{"live-1", "live-2", "live-3"} {
		if _, ok := c.entries[k]; !ok {
			t.Errorf("live entry %s must survive while expired entries exist", k)
		}
	}
}

// TestTTLCacheEvictsOldestWhenAllLive: with no expired entries at capacity,
// the entry closest to expiring is dropped — never the key being put.
func TestTTLCacheEvictsOldestWhenAllLive(t *testing.T) {
	c := newTTLCache[struct{}](time.Minute)
	c.max = 2
	c.put("a", struct{}{}, nil)
	c.put("b", struct{}{}, nil)
	c.entries["a"] = ttlCacheEntry[struct{}]{until: time.Now()} // a is the oldest
	c.put("c", struct{}{}, nil)
	if _, ok := c.entries["a"]; ok {
		t.Error("the entry closest to expiring (a) should be evicted first")
	}
	if _, ok := c.entries["c"]; !ok {
		t.Error("the just-put key (c) must be present")
	}
	if got := len(c.entries); got != 2 {
		t.Errorf("entries = %d, want exactly max 2", got)
	}
}

// TestResolverCacheOffByDefault: the plain constructor stays uncached so
// existing callers and tests see store edits on the very next call.
func TestResolverCacheOffByDefault(t *testing.T) {
	s := newCountingStore()
	seedCountingDefaults(s)
	r := NewResolver(s)
	ctx := context.Background()
	for range 3 {
		if _, err := r.Resolve(ctx, "u1"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if userTeam, _, _, _ := s.totals(); userTeam != 3 {
		t.Errorf("userTeam reads = %d, want 3 (uncached)", userTeam)
	}
}
