// Package routing implements model-routing (design D14): provider/model
// selection, credential resolution (platform key default, team key override),
// and failover across providers. Quota metering data is recorded via a Meter.
package routing

import (
	"context"
	"errors"

	"nowhere-agent/internal/provider"
)

// ErrNoProvider is returned when no configured provider can serve a request.
var ErrNoProvider = errors.New("no provider available")

// Credentials holds an API key and the quota scope it bills against.
type Credentials struct {
	APIKey    string
	// TeamID is set when a team key is used; empty for platform key.
	TeamID    string
	Platform  bool
}

// KeyStore resolves credentials for a user, preferring a team key.
type KeyStore interface {
	// Resolve returns credentials for the user: the user's team key if one is
	// configured, otherwise the platform key.
	Resolve(ctx context.Context, userID string) (Credentials, error)
}

// Target is a resolved provider+model+credentials triple.
type Target struct {
	Provider    string
	Model       string
	Credentials Credentials
}

// Router picks a provider+model for a request and resolves credentials, with
// failover ordering. Adapters are resolved lazily from the provider registry.
type Router struct {
	registry *provider.Registry
	keys     KeyStore
	// fallbacks is the ordered list of provider names to try.
	fallbacks []string
}

// NewRouter creates a Router with an ordered failover list of provider names.
func NewRouter(registry *provider.Registry, keys KeyStore, fallbacks []string) *Router {
	return &Router{registry: registry, keys: keys, fallbacks: fallbacks}
}

// Resolve picks the first available provider and resolves credentials.
func (r *Router) Resolve(ctx context.Context, userID string) (Target, error) {
	creds, err := r.keys.Resolve(ctx, userID)
	if err != nil {
		return Target{}, err
	}
	for _, name := range r.fallbacks {
		if _, err := r.registry.Get(name); err == nil {
			return Target{Provider: name, Credentials: creds}, nil
		}
	}
	return Target{}, ErrNoProvider
}

// Adapter returns the adapter for a target's provider.
func (r *Router) Adapter(t Target) (provider.Adapter, error) {
	return r.registry.Get(t.Provider)
}
