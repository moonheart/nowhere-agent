package providerreg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"nowhere-agent/internal/secrets"
)

// PGStore is the Postgres-backed provider registry. Keys are encrypted at rest
// with the injected Encryptor; nil stores plaintext (legacy/dev, same
// semantics as the identity key store).
type PGStore struct {
	db  *sql.DB
	enc *secrets.Encryptor
}

// NewPGStore creates a Postgres-backed Store.
func NewPGStore(db *sql.DB) *PGStore {
	return &PGStore{db: db}
}

// WithEncryption enables encryption-at-rest for provider keys.
func (s *PGStore) WithEncryption(enc *secrets.Encryptor) *PGStore {
	s.enc = enc
	return s
}

var _ Store = (*PGStore)(nil)

const providerCols = `id, scope, COALESCE(team_id::text, ''), name, vendor, base_url, api_key, is_default, enabled, created_at, updated_at`

const modelCols = `id, provider_id, name, display_name, vision, context_window, price_input_per_mtok, price_output_per_mtok, price_cache_read_per_mtok, is_default, enabled, created_at, updated_at`

func scanProvider(row interface{ Scan(...any) error }) (Provider, error) {
	var p Provider
	err := row.Scan(&p.ID, &p.Scope, &p.TeamID, &p.Name, &p.Vendor, &p.BaseURL, &p.RawKey, &p.IsDefault, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func scanModel(row interface{ Scan(...any) error }) (Model, error) {
	var m Model
	var cw sql.NullInt64
	var pi, po, pcr sql.NullFloat64
	err := row.Scan(&m.ID, &m.ProviderID, &m.Name, &m.DisplayName, &m.Vision, &cw, &pi, &po, &pcr, &m.IsDefault, &m.Enabled, &m.CreatedAt, &m.UpdatedAt)
	if cw.Valid {
		m.ContextWindow = &cw.Int64
	}
	if pi.Valid {
		m.PriceInput = &pi.Float64
	}
	if po.Valid {
		m.PriceOutput = &po.Float64
	}
	if pcr.Valid {
		m.PriceCacheRead = &pcr.Float64
	}
	return m, err
}

func (s *PGStore) encrypt(plain string) (string, error) {
	if s.enc == nil {
		return plain, nil
	}
	return s.enc.Encrypt(plain)
}

func (s *PGStore) decrypt(stored string) (string, error) {
	if s.enc == nil {
		return stored, nil
	}
	return s.enc.Decrypt(stored)
}

func (s *PGStore) decryptProvider(p Provider) (Provider, error) {
	if p.RawKey == "" {
		return p, nil
	}
	plain, err := s.decrypt(p.RawKey)
	if err != nil {
		return Provider{}, fmt.Errorf("decrypt provider key: %w", err)
	}
	p.RawKey = plain
	return p, nil
}

// ListSystemProviders returns every system-scoped provider, oldest first.
func (s *PGStore) ListSystemProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+providerCols+` FROM providers
		WHERE scope = $1
		ORDER BY created_at`, ScopeSystem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProviders(rows, s)
}

// ListTeamProviders returns one team's own providers.
func (s *PGStore) ListTeamProviders(ctx context.Context, teamID string) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+providerCols+` FROM providers
		WHERE scope = $1 AND team_id = $2
		ORDER BY created_at`, ScopeTeam, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProviders(rows, s)
}

// VisibleToTeam returns system providers plus the team's own providers.
func (s *PGStore) VisibleToTeam(ctx context.Context, teamID string) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+providerCols+` FROM providers
		WHERE scope = $1 OR (scope = $2 AND team_id = $3)
		ORDER BY scope, created_at`, ScopeSystem, ScopeTeam, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProviders(rows, s)
}

func collectProviders(rows *sql.Rows, s *PGStore) ([]Provider, error) {
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		p, err = s.decryptProvider(p)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider returns one provider with its decrypted key.
func (s *PGStore) GetProvider(ctx context.Context, id string) (Provider, error) {
	p, err := scanProvider(s.db.QueryRowContext(ctx, `
		SELECT `+providerCols+` FROM providers WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	if err != nil {
		return Provider{}, err
	}
	return s.decryptProvider(p)
}

// CreateProvider inserts a provider. Scope and team_id are fixed at creation.
func (s *PGStore) CreateProvider(ctx context.Context, p Provider) (Provider, error) {
	if p.Scope != ScopeSystem && p.Scope != ScopeTeam {
		return Provider{}, fmt.Errorf("invalid scope %q", p.Scope)
	}
	if p.Scope == ScopeTeam && p.TeamID == "" {
		return Provider{}, fmt.Errorf("team provider requires team_id")
	}
	key, err := s.encrypt(p.RawKey)
	if err != nil {
		return Provider{}, err
	}
	created, err := scanProvider(s.db.QueryRowContext(ctx, `
		INSERT INTO providers (scope, team_id, name, vendor, base_url, api_key, is_default, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+providerCols,
		p.Scope, nullIfEmpty(p.TeamID), p.Name, p.Vendor, p.BaseURL, key, p.IsDefault, p.Enabled))
	if err != nil {
		if isUniqueViolation(err) {
			return Provider{}, ErrNameConflict
		}
		if isCheckViolation(err) {
			return Provider{}, fmt.Errorf("provider %q: %w", p.Name, err)
		}
		return Provider{}, err
	}
	return s.decryptProvider(created)
}

// UpdateProvider applies the non-nil fields of upd. Scope, team_id, and
// is_default are immutable here (use SetDefaultProvider for the default).
func (s *PGStore) UpdateProvider(ctx context.Context, id string, upd ProviderUpdate) (Provider, error) {
	sets, args := []string{}, []any{}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if upd.Name != nil {
		add("name", *upd.Name)
	}
	if upd.Vendor != nil {
		add("vendor", *upd.Vendor)
	}
	if upd.BaseURL != nil {
		add("base_url", *upd.BaseURL)
	}
	if upd.Key != nil {
		key, err := s.encrypt(*upd.Key)
		if err != nil {
			return Provider{}, err
		}
		add("api_key", key)
	}
	if upd.Enabled != nil {
		add("enabled", *upd.Enabled)
	}
	if len(sets) == 0 {
		return s.GetProvider(ctx, id)
	}
	args = append(args, id)
	query := fmt.Sprintf(`
		UPDATE providers SET %s, updated_at = now()
		WHERE id = $%d
		RETURNING `+providerCols, strings.Join(sets, ", "), len(args))
	updated, err := scanProvider(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	if isUniqueViolation(err) {
		return Provider{}, ErrNameConflict
	}
	if err != nil {
		return Provider{}, err
	}
	return s.decryptProvider(updated)
}

// DeleteProvider removes a provider, rejecting deletion of the platform default
// or a provider a team assigns to.
func (s *PGStore) DeleteProvider(ctx context.Context, id string) error {
	p, err := s.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if p.IsDefault {
		return fmt.Errorf("%w: clear the platform default first", ErrProviderInUse)
	}
	var assigned int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM team_provider_settings WHERE provider_id = $1`, id).Scan(&assigned); err != nil {
		return err
	}
	if assigned > 0 {
		return fmt.Errorf("%w: clear the team assignment first", ErrProviderInUse)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE id = $1`, id); err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: clear the team assignment first", ErrProviderInUse)
		}
		return err
	}
	return nil
}

// SetDefaultProvider marks one system provider as the platform default,
// clearing any previous default in the same transaction.
func (s *PGStore) SetDefaultProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE providers SET is_default = false, updated_at = now()
		WHERE scope = $1 AND is_default`, ScopeSystem); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE providers SET is_default = true, updated_at = now()
		WHERE id = $1 AND scope = $2`, id, ScopeSystem)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := s.GetProvider(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("%w: only system providers can be the default", ErrModelMismatch)
	}
	return tx.Commit()
}

// ListModels returns a provider's models.
func (s *PGStore) ListModels(ctx context.Context, providerID string) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+modelCols+` FROM provider_models
		WHERE provider_id = $1
		ORDER BY created_at`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModel returns one model.
func (s *PGStore) GetModel(ctx context.Context, id string) (Model, error) {
	m, err := scanModel(s.db.QueryRowContext(ctx, `
		SELECT `+modelCols+` FROM provider_models WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, ErrNotFound
	}
	return m, err
}

// CreateModel inserts a model under a provider.
func (s *PGStore) CreateModel(ctx context.Context, m Model) (Model, error) {
	created, err := scanModel(s.db.QueryRowContext(ctx, `
		INSERT INTO provider_models (provider_id, name, display_name, vision, context_window, price_input_per_mtok, price_output_per_mtok, price_cache_read_per_mtok, is_default, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+modelCols,
		m.ProviderID, m.Name, m.DisplayName, m.Vision, int64Ptr(m.ContextWindow),
		float64Ptr(m.PriceInput), float64Ptr(m.PriceOutput), float64Ptr(m.PriceCacheRead),
		m.IsDefault, m.Enabled))
	if err != nil {
		if isUniqueViolation(err) {
			return Model{}, ErrNameConflict
		}
		if isCheckViolation(err) {
			return Model{}, fmt.Errorf("model %q: %w", m.Name, err)
		}
		return Model{}, err
	}
	return created, nil
}

// UpdateModel applies the non-nil fields of upd.
func (s *PGStore) UpdateModel(ctx context.Context, id string, upd ModelUpdate) (Model, error) {
	sets, args := []string{}, []any{}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if upd.Name != nil {
		add("name", *upd.Name)
	}
	if upd.DisplayName != nil {
		add("display_name", *upd.DisplayName)
	}
	if upd.Vision != nil {
		add("vision", *upd.Vision)
	}
	if upd.ContextWindow != nil {
		add("context_window", int64Ptr(upd.ContextWindow))
	} else if upd.ClearContext {
		add("context_window", nil)
	}
	if upd.PriceInput != nil {
		add("price_input_per_mtok", *upd.PriceInput)
	}
	if upd.PriceOutput != nil {
		add("price_output_per_mtok", *upd.PriceOutput)
	}
	if upd.PriceCacheRead != nil {
		add("price_cache_read_per_mtok", *upd.PriceCacheRead)
	}
	if upd.ClearPrices {
		add("price_input_per_mtok", nil)
		add("price_output_per_mtok", nil)
		add("price_cache_read_per_mtok", nil)
	}
	if upd.Enabled != nil {
		add("enabled", *upd.Enabled)
	}
	if len(sets) == 0 {
		return s.GetModel(ctx, id)
	}
	args = append(args, id)
	query := fmt.Sprintf(`
		UPDATE provider_models SET %s, updated_at = now()
		WHERE id = $%d
		RETURNING `+modelCols, strings.Join(sets, ", "), len(args))
	updated, err := scanModel(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, ErrNotFound
	}
	if isUniqueViolation(err) {
		return Model{}, ErrNameConflict
	}
	if err != nil {
		return Model{}, err
	}
	return updated, nil
}

// DeleteModel removes a model, rejecting deletion of a provider's default model
// or a model a team assignment references.
func (s *PGStore) DeleteModel(ctx context.Context, id string) error {
	m, err := s.GetModel(ctx, id)
	if err != nil {
		return err
	}
	if m.IsDefault {
		return fmt.Errorf("%w: clear the provider default first", ErrModelInUse)
	}
	var assigned int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM team_provider_settings WHERE model_id = $1`, id).Scan(&assigned); err != nil {
		return err
	}
	if assigned > 0 {
		return fmt.Errorf("%w: clear the team assignment first", ErrModelInUse)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM provider_models WHERE id = $1`, id); err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: clear the team assignment first", ErrModelInUse)
		}
		return err
	}
	return nil
}

// SetDefaultModel marks one model as its provider's default, clearing the
// previous default in the same transaction.
func (s *PGStore) SetDefaultModel(ctx context.Context, providerID, modelID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE provider_models SET is_default = false, updated_at = now()
		WHERE provider_id = $1 AND is_default`, providerID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE provider_models SET is_default = true, updated_at = now()
		WHERE id = $1 AND provider_id = $2`, modelID, providerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: model does not belong to provider", ErrModelMismatch)
	}
	return tx.Commit()
}

// GetTeamAssignment returns a team's assignment.
func (s *PGStore) GetTeamAssignment(ctx context.Context, teamID string) (TeamAssignment, error) {
	var a TeamAssignment
	var modelID sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT team_id, provider_id, model_id, created_at, updated_at
		FROM team_provider_settings WHERE team_id = $1`, teamID).
		Scan(&a.TeamID, &a.ProviderID, &modelID, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamAssignment{}, ErrNotFound
	}
	if err != nil {
		return TeamAssignment{}, err
	}
	a.ModelID = modelID.String
	return a, nil
}

// SetTeamAssignment records the team's selected provider and default model. The
// provider must be enabled; the model, when given, must belong to the provider
// and be enabled. An empty modelID means "the provider's default model".
func (s *PGStore) SetTeamAssignment(ctx context.Context, teamID, providerID, modelID string) error {
	p, err := s.GetProvider(ctx, providerID)
	if err != nil {
		return err
	}
	if !p.Enabled {
		return ErrProviderDisabled
	}
	if modelID != "" {
		m, err := s.GetModel(ctx, modelID)
		if err != nil {
			return err
		}
		if m.ProviderID != providerID {
			return ErrModelMismatch
		}
		if !m.Enabled {
			return ErrProviderDisabled
		}
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO team_provider_settings (team_id, provider_id, model_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (team_id)
		DO UPDATE SET provider_id = EXCLUDED.provider_id, model_id = EXCLUDED.model_id, updated_at = now()`,
		teamID, providerID, nullIfEmpty(modelID))
	return err
}

// ClearTeamAssignment removes the team's selection, returning to the platform
// default. It reports whether an assignment was there to clear.
func (s *PGStore) ClearTeamAssignment(ctx context.Context, teamID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM team_provider_settings WHERE team_id = $1`, teamID)
	return err
}

// PlatformDefault returns the default system provider, or the first enabled
// system provider when none is marked default (a sensible bootstrap fallback).
func (s *PGStore) PlatformDefault(ctx context.Context) (Provider, error) {
	p, err := scanProvider(s.db.QueryRowContext(ctx, `
		SELECT `+providerCols+` FROM providers
		WHERE scope = $1 AND is_default AND enabled
		ORDER BY created_at LIMIT 1`, ScopeSystem))
	if err == nil {
		return s.decryptProvider(p)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Provider{}, err
	}
	p, err = scanProvider(s.db.QueryRowContext(ctx, `
		SELECT `+providerCols+` FROM providers
		WHERE scope = $1 AND enabled
		ORDER BY created_at LIMIT 1`, ScopeSystem))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, ErrNoProvider
	}
	if err != nil {
		return Provider{}, err
	}
	return s.decryptProvider(p)
}

// UserTeam returns the team id used for resolution — the lowest team id among
// the user's memberships, deterministic when a user belongs to several — or ""
// when the user has none.
func (s *PGStore) UserTeam(ctx context.Context, userID string) (string, error) {
	var teamID string
	err := s.db.QueryRowContext(ctx, `
		SELECT team_id::text FROM team_memberships
		WHERE user_id = $1
		ORDER BY team_id
		LIMIT 1`, userID).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return teamID, err
}

// EnabledModel resolves a model by name under a provider, enabled only.
func (s *PGStore) EnabledModel(ctx context.Context, providerID, name string) (Model, error) {
	m, err := scanModel(s.db.QueryRowContext(ctx, `
		SELECT `+modelCols+` FROM provider_models
		WHERE provider_id = $1 AND name = $2 AND enabled`, providerID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, ErrNotFound
	}
	return m, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func int64Ptr(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func float64Ptr(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

func isCheckViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "violates check constraint")
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "violates foreign key constraint")
}
