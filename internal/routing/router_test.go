package routing

import (
	"context"
	"errors"
	"testing"

	"nowhere-agent/internal/provider"
)

type staticKeys struct {
	creds map[string]Credentials // userID -> creds
	err   error
}

func (s staticKeys) Resolve(_ context.Context, userID string) (Credentials, error) {
	if s.err != nil {
		return Credentials{}, s.err
	}
	if c, ok := s.creds[userID]; ok {
		return c, nil
	}
	return Credentials{APIKey: "platform-key", Platform: true}, nil
}

type fakeAdapter struct{ name string }

func (f fakeAdapter) Name() string { return f.name }
func (f fakeAdapter) Stream(provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event)
	close(ch)
	return ch, nil
}

func registryWith(names ...string) *provider.Registry {
	r := provider.NewRegistry()
	for _, n := range names {
		r.Register(fakeAdapter{name: n})
	}
	return r
}

func TestResolvePrefersFirstAvailable(t *testing.T) {
	r := NewRouter(registryWith("openai"), staticKeys{}, []string{"anthropic", "openai"})
	tgt, err := r.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	// anthropic not registered → falls back to openai.
	if tgt.Provider != "openai" {
		t.Errorf("provider = %q want openai", tgt.Provider)
	}
}

func TestResolvePlatformKeyByDefault(t *testing.T) {
	r := NewRouter(registryWith("anthropic"), staticKeys{}, []string{"anthropic"})
	tgt, err := r.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !tgt.Credentials.Platform || tgt.Credentials.APIKey != "platform-key" {
		t.Errorf("expected platform key, got %+v", tgt.Credentials)
	}
}

func TestResolveTeamKeyOverride(t *testing.T) {
	keys := staticKeys{creds: map[string]Credentials{
		"u1": {APIKey: "team-key", TeamID: "team-1"},
	}}
	r := NewRouter(registryWith("anthropic"), keys, []string{"anthropic"})
	tgt, err := r.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Credentials.TeamID != "team-1" || tgt.Credentials.APIKey != "team-key" {
		t.Errorf("expected team key, got %+v", tgt.Credentials)
	}
}

func TestResolveNoProvider(t *testing.T) {
	r := NewRouter(registryWith(), staticKeys{}, []string{"anthropic"})
	if _, err := r.Resolve(context.Background(), "u1"); err != ErrNoProvider {
		t.Errorf("expected ErrNoProvider, got %v", err)
	}
}

func TestResolveKeyStoreError(t *testing.T) {
	keys := staticKeys{err: errors.New("keystore down")}
	r := NewRouter(registryWith("anthropic"), keys, []string{"anthropic"})
	if _, err := r.Resolve(context.Background(), "u1"); err == nil {
		t.Error("expected keystore error to propagate")
	}
}

func TestAdapterLookup(t *testing.T) {
	r := NewRouter(registryWith("anthropic"), staticKeys{}, []string{"anthropic"})
	a, err := r.Adapter(Target{Provider: "anthropic"})
	if err != nil || a.Name() != "anthropic" {
		t.Errorf("adapter = %v err %v", a, err)
	}
}
