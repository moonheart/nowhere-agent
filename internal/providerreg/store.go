// Package providerreg implements the DB-managed provider registry (change
// provider-registry): system- and team-scoped LLM providers, their models, and
// per-team provider/model assignment. It replaces the env-var model selection
// (LLM_*/VISION_*) and the deprecated team_api_keys mechanism.
//
// Two scopes share one providers table:
//
//	system — platform-managed, visible to every team; one of them is the
//	        platform default.
//	team   — owned by one team (providers.team_id), visible only to that
//	        team's members, managed by its owners/administrators.
//
// Provider API keys are encrypted at rest (secrets.Encryptor) and masked in
// every read; plaintext is exposed only to the resolver, which feeds the
// adapter factory.
package providerreg

import (
	"context"
	"errors"
	"time"
)

// Scopes.
const (
	ScopeSystem = "system"
	ScopeTeam   = "team"
)

// Vendors the adapters speak.
const (
	VendorAnthropic = "anthropic"
	VendorOpenAI    = "openai"
)

// Errors surfaced across the store boundary. Callers map them to status codes
// (404/409) or to resolution failures.
var (
	// ErrNotFound reports a missing provider, model, or assignment.
	ErrNotFound = errors.New("providerreg: not found")
	// ErrNameConflict reports a name that collides within its scope.
	ErrNameConflict = errors.New("providerreg: name already exists")
	// ErrDefaultConflict reports a second default when one is already set.
	ErrDefaultConflict = errors.New("providerreg: a default is already set")
	// ErrProviderInUse reports deleting a provider a team assignment uses.
	ErrProviderInUse = errors.New("providerreg: provider is assigned to a team")
	// ErrModelInUse reports deleting a model that is a default or assignment.
	ErrModelInUse = errors.New("providerreg: model is a default or assigned")
	// ErrProviderDisabled reports assigning a disabled provider or model.
	ErrProviderDisabled = errors.New("providerreg: provider or model is disabled")
	// ErrModelMismatch reports a model that does not belong to the provider.
	ErrModelMismatch = errors.New("providerreg: model does not belong to provider")
)

// Provider is one LLM provider in the registry. RawKey is the (decrypted) API
// key; display paths MUST mask it via MaskKey before serialization.
type Provider struct {
	ID        string
	Scope     string // system | team
	TeamID    string // empty for system scope
	Name      string
	Vendor    string // anthropic | openai
	BaseURL   string
	RawKey    string
	IsDefault bool
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Model is one model under a provider.
type Model struct {
	ID            string
	ProviderID    string
	Name          string
	DisplayName   string
	Vision        bool
	ContextWindow *int64 // nil = derive from the capability table
	// Price* are the model's price in USD per MILLION tokens for each billable
	// counter (input, output, cache-read), used by the usage report to turn
	// tokens into money. nil = unpriced (cost estimates count it as zero).
	PriceInput     *float64
	PriceOutput    *float64
	PriceCacheRead *float64
	IsDefault      bool
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// TeamAssignment is a team's selected provider and default model.
type TeamAssignment struct {
	TeamID     string
	ProviderID string
	// ModelID is empty when the team uses the provider's default model.
	ModelID   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProviderUpdate carries the mutable provider fields. Zero values are skipped
// so a partial write never clobbers a field the caller did not intend.
type ProviderUpdate struct {
	Name    *string
	Vendor  *string
	BaseURL *string
	// Key, when non-nil, replaces the stored key; empty string clears it.
	Key     *string
	Enabled *bool
}

// ModelUpdate carries the mutable model fields.
type ModelUpdate struct {
	Name          *string
	DisplayName   *string
	Vision        *bool
	ContextWindow *int64 // nil = no change
	ClearContext  bool   // drop the override and derive from the capability table
	// Price* are nil = no change; ClearPrices drops all three back to unpriced.
	PriceInput     *float64
	PriceOutput    *float64
	PriceCacheRead *float64
	ClearPrices    bool
	Enabled        *bool
}

// Store is the provider registry boundary. Implementations are symmetric (PG in
// production; tests may use the same PG store). All reads return masked-safe
// domain values with RawKey populated; display layers must mask before leaking
// a key to a client.
type Store interface {
	// Providers.
	ListSystemProviders(ctx context.Context) ([]Provider, error)
	ListTeamProviders(ctx context.Context, teamID string) ([]Provider, error)
	// VisibleToTeam returns system providers plus the team's own providers.
	VisibleToTeam(ctx context.Context, teamID string) ([]Provider, error)
	GetProvider(ctx context.Context, id string) (Provider, error)
	CreateProvider(ctx context.Context, p Provider) (Provider, error)
	UpdateProvider(ctx context.Context, id string, upd ProviderUpdate) (Provider, error)
	DeleteProvider(ctx context.Context, id string) error
	// SetDefaultProvider marks one system provider as the platform default.
	SetDefaultProvider(ctx context.Context, id string) error

	// Models.
	ListModels(ctx context.Context, providerID string) ([]Model, error)
	GetModel(ctx context.Context, id string) (Model, error)
	CreateModel(ctx context.Context, m Model) (Model, error)
	UpdateModel(ctx context.Context, id string, upd ModelUpdate) (Model, error)
	DeleteModel(ctx context.Context, id string) error
	SetDefaultModel(ctx context.Context, providerID, modelID string) error

	// Team assignment.
	GetTeamAssignment(ctx context.Context, teamID string) (TeamAssignment, error)
	SetTeamAssignment(ctx context.Context, teamID, providerID, modelID string) error
	ClearTeamAssignment(ctx context.Context, teamID string) error

	// Resolution helpers.
	// PlatformDefault returns the default system provider, or the first enabled
	// system provider when none is marked default.
	PlatformDefault(ctx context.Context) (Provider, error)
	// UserTeam returns the team id used for resolution: the lowest team id among
	// the user's memberships (deterministic), or "" when the user has none.
	UserTeam(ctx context.Context, userID string) (string, error)
	// EnabledModel resolves a model name under a provider to its row.
	EnabledModel(ctx context.Context, providerID, name string) (Model, error)
}
