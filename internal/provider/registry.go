package provider

import "fmt"

// Registry maps provider names to adapters. Model routing (design D14) uses it
// to resolve a provider+model selection to a concrete adapter.
type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

// Register adds an adapter, keyed by its Name().
func (r *Registry) Register(a Adapter) {
	r.adapters[a.Name()] = a
}

// Get returns the adapter for a provider name.
func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("provider %q not registered", name)
	}
	return a, nil
}

// Names returns the registered provider names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		names = append(names, n)
	}
	return names
}
