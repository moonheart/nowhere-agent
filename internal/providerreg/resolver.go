package providerreg

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoProvider is returned by the resolver when no enabled provider can serve
// a request (nothing configured, or the team's assignment is not servable).
var ErrNoProvider = errors.New("no provider available")

// ErrUnknownModel is returned by ResolveModel when a reference names no enabled
// model on the resolved provider (fail-closed: never silently substitute).
var ErrUnknownModel = errors.New("model not enabled on the resolved provider")

// Target is the fully-resolved provider+model+credential triple a run needs to
// build its adapter: vendor/base URL from the provider row, the decrypted API
// key, and the model name to send to the provider API.
type Target struct {
	ProviderID string
	Vendor     string
	BaseURL    string
	APIKey     string
	Model      string
}

// Resolver turns a store into per-request provider+model decisions. Built once
// at boot over the registry store; every decision runs on the request path so
// registry edits and team reassignments take effect without a restart.
type Resolver struct {
	store Store
}

// NewResolver creates a Resolver over a Store.
func NewResolver(store Store) *Resolver {
	return &Resolver{store: store}
}

// Resolve picks the provider+model for a user's chat run: the user's team
// assignment (a system or team-owned provider and its default model) when one
// exists, otherwise the platform default provider and its default model.
func (r *Resolver) Resolve(ctx context.Context, userID string) (Target, error) {
	teamID, err := r.store.UserTeam(ctx, userID)
	if err != nil {
		return Target{}, err
	}
	if teamID != "" {
		if t, err := r.resolveTeam(ctx, teamID); err == nil {
			return t, nil
		} else if !errors.Is(err, ErrNoProvider) {
			return Target{}, err
		}
	}
	return r.platformDefault(ctx)
}

// ResolveForTeam is Resolve for the schedule trigger, which holds a task's team
// id rather than a chat caller. An empty teamID resolves the platform default.
func (r *Resolver) ResolveForTeam(ctx context.Context, teamID string) (Target, error) {
	if teamID != "" {
		if t, err := r.resolveTeam(ctx, teamID); err == nil {
			return t, nil
		} else if !errors.Is(err, ErrNoProvider) {
			return Target{}, err
		}
	}
	return r.platformDefault(ctx)
}

func (r *Resolver) resolveTeam(ctx context.Context, teamID string) (Target, error) {
	a, err := r.store.GetTeamAssignment(ctx, teamID)
	if errors.Is(err, ErrNotFound) {
		return Target{}, ErrNoProvider
	}
	if err != nil {
		return Target{}, err
	}
	p, err := r.store.GetProvider(ctx, a.ProviderID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Target{}, ErrNoProvider
		}
		return Target{}, err
	}
	if !p.Enabled {
		return Target{}, ErrNoProvider
	}
	model, err := r.modelFor(ctx, p, a.ModelID)
	if err != nil {
		return Target{}, err
	}
	return Target{
		ProviderID: p.ID,
		Vendor:     p.Vendor,
		BaseURL:    p.BaseURL,
		APIKey:     p.RawKey,
		Model:      model,
	}, nil
}

func (r *Resolver) platformDefault(ctx context.Context) (Target, error) {
	p, err := r.store.PlatformDefault(ctx)
	if err != nil {
		return Target{}, err
	}
	model, err := r.modelFor(ctx, p, "")
	if err != nil {
		return Target{}, err
	}
	return Target{
		ProviderID: p.ID,
		Vendor:     p.Vendor,
		BaseURL:    p.BaseURL,
		APIKey:     p.RawKey,
		Model:      model,
	}, nil
}

// modelFor resolves the model a provider serves: the provider's default model
// when modelID is empty, otherwise that specific model (falling back to the
// default if it was removed). A provider with no enabled model is unservable.
func (r *Resolver) modelFor(ctx context.Context, p Provider, modelID string) (string, error) {
	if modelID != "" {
		if m, err := r.store.GetModel(ctx, modelID); err == nil && m.Enabled && m.ProviderID == p.ID {
			return m.Name, nil
		}
	}
	models, err := r.store.ListModels(ctx, p.ID)
	if err != nil {
		return "", err
	}
	for _, m := range models {
		if m.Enabled && m.IsDefault {
			return m.Name, nil
		}
	}
	for _, m := range models {
		if m.Enabled {
			return m.Name, nil
		}
	}
	return "", fmt.Errorf("%w: provider %q has no enabled model", ErrNoProvider, p.Name)
}

// ResolveModel maps an explicit model reference (a scheduled task's or agent
// definition's model string) onto the resolved provider. Fail-closed: an empty
// reference returns the target's default model, and a reference that names no
// enabled model returns ErrUnknownModel instead of substituting.
func (r *Resolver) ResolveModel(ctx context.Context, t Target, name string) (string, error) {
	if name == "" {
		return t.Model, nil
	}
	if _, err := r.store.EnabledModel(ctx, t.ProviderID, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("%w: %q", ErrUnknownModel, name)
		}
		return "", err
	}
	return name, nil
}

// VisionModel returns the model name the view_image tool should use for a
// target: the provider's default vision-capable model, or the first
// vision-capable model when none is marked default. The second return is false
// when the provider has no vision-capable model (the tool is then unavailable).
func (r *Resolver) VisionModel(ctx context.Context, t Target) (string, bool) {
	models, err := r.store.ListModels(ctx, t.ProviderID)
	if err != nil {
		return "", false
	}
	for _, m := range models {
		if m.Enabled && m.Vision && m.IsDefault {
			return m.Name, true
		}
	}
	for _, m := range models {
		if m.Enabled && m.Vision {
			return m.Name, true
		}
	}
	return "", false
}
