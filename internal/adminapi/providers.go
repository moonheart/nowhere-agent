package adminapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/providerreg"
)

// Provider registry routes (change provider-registry). Two tiers share one
// store: platform admins manage system providers and their models (one of them
// is the platform default), and team admins manage their team's own providers
// plus their team's assignment to any visible provider (system or team-owned).
// API keys are never serialized; display paths mask them via providerreg.MaskKey
// and the audit trail records rotations, not the secret itself.

// ---- DTOs ----

type providerDTO struct {
	ID        string     `json:"id"`
	Scope     string     `json:"scope"`
	TeamID    string     `json:"team_id,omitempty"`
	Name      string     `json:"name"`
	Vendor    string     `json:"vendor"`
	BaseURL   string     `json:"base_url,omitempty"`
	Key       string     `json:"key"`
	IsDefault bool       `json:"is_default"`
	Enabled   bool       `json:"enabled"`
	Models    []modelDTO `json:"models,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type modelDTO struct {
	ID            string    `json:"id"`
	ProviderID    string    `json:"provider_id"`
	Name          string    `json:"name"`
	DisplayName   string    `json:"display_name,omitempty"`
	Vision        bool      `json:"vision"`
	ContextWindow *int64    `json:"context_window,omitempty"`
	IsDefault     bool      `json:"is_default"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func providerDTOs(providers []providerreg.Provider, withModels bool) []providerDTO {
	out := make([]providerDTO, 0, len(providers))
	for _, p := range providers {
		out = append(out, providerDTO{
			ID:        p.ID,
			Scope:     p.Scope,
			TeamID:    p.TeamID,
			Name:      p.Name,
			Vendor:    p.Vendor,
			BaseURL:   p.BaseURL,
			Key:       providerreg.MaskKey(p.RawKey),
			IsDefault: p.IsDefault,
			Enabled:   p.Enabled,
			CreatedAt: p.CreatedAt,
			UpdatedAt: p.UpdatedAt,
		})
	}
	return out
}

func (h *Handler) providerDTO(ctx context.Context, p providerreg.Provider) (providerDTO, error) {
	d := providerDTO{
		ID:        p.ID,
		Scope:     p.Scope,
		TeamID:    p.TeamID,
		Name:      p.Name,
		Vendor:    p.Vendor,
		BaseURL:   p.BaseURL,
		Key:       providerreg.MaskKey(p.RawKey),
		IsDefault: p.IsDefault,
		Enabled:   p.Enabled,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
	if h.providers != nil {
		models, err := h.providers.ListModels(ctx, p.ID)
		if err != nil {
			return providerDTO{}, err
		}
		d.Models = make([]modelDTO, 0, len(models))
		for _, m := range models {
			d.Models = append(d.Models, modelDTO{
				ID:            m.ID,
				ProviderID:    m.ProviderID,
				Name:          m.Name,
				DisplayName:   m.DisplayName,
				Vision:        m.Vision,
				ContextWindow: m.ContextWindow,
				IsDefault:     m.IsDefault,
				Enabled:       m.Enabled,
				CreatedAt:     m.CreatedAt,
				UpdatedAt:     m.UpdatedAt,
			})
		}
	}
	return d, nil
}

func modelDTOs(models []providerreg.Model) []modelDTO {
	out := make([]modelDTO, 0, len(models))
	for _, m := range models {
		out = append(out, modelDTO{
			ID:            m.ID,
			ProviderID:    m.ProviderID,
			Name:          m.Name,
			DisplayName:   m.DisplayName,
			Vision:        m.Vision,
			ContextWindow: m.ContextWindow,
			IsDefault:     m.IsDefault,
			Enabled:       m.Enabled,
			CreatedAt:     m.CreatedAt,
			UpdatedAt:     m.UpdatedAt,
		})
	}
	return out
}

// requireProviders answers 503 when the registry is not wired, mirroring the
// other optional stores (memory, quotas).
func (h *Handler) requireProviders(w http.ResponseWriter) bool {
	if h.providers == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry unavailable")
		return false
	}
	return true
}

type createProviderRequest struct {
	Name    string `json:"name"`
	Vendor  string `json:"vendor"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Enabled *bool  `json:"enabled"`
}

type updateProviderRequest struct {
	Name    *string `json:"name"`
	Vendor  *string `json:"vendor"`
	BaseURL *string `json:"base_url"`
	APIKey  *string `json:"api_key"`
	Enabled *bool   `json:"enabled"`
}

func validateVendor(w http.ResponseWriter, vendor string) bool {
	switch vendor {
	case providerreg.VendorAnthropic, providerreg.VendorOpenAI:
		return true
	default:
		writeError(w, http.StatusBadRequest, "vendor must be anthropic or openai")
		return false
	}
}

// ---- system tier (platform admin) ----

func (h *Handler) listSystemProviders(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	providers, err := h.providers.ListSystemProviders(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]providerDTO, 0, len(providers))
	for _, p := range providers {
		d, err := h.providerDTO(r.Context(), p)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (h *Handler) createSystemProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	var req createProviderRequest
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if !validateVendor(w, req.Vendor) {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	p, err := h.providers.CreateProvider(r.Context(), providerreg.Provider{
		Scope:   providerreg.ScopeSystem,
		Name:    req.Name,
		Vendor:  req.Vendor,
		BaseURL: req.BaseURL,
		RawKey:  req.APIKey,
		Enabled: enabled,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	d, err := h.providerDTO(r.Context(), p)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderCreate).Target("provider", p.ID).Detail(map[string]any{"name": p.Name, "vendor": p.Vendor}))
	writeJSON(w, http.StatusCreated, map[string]any{"provider": d})
}

func (h *Handler) updateSystemProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	var req updateProviderRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Vendor != nil && !validateVendor(w, *req.Vendor) {
		return
	}
	p, err := h.providers.UpdateProvider(r.Context(), r.PathValue("pid"), providerreg.ProviderUpdate{
		Name:    req.Name,
		Vendor:  req.Vendor,
		BaseURL: req.BaseURL,
		Key:     req.APIKey,
		Enabled: req.Enabled,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	d, err := h.providerDTO(r.Context(), p)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	detail := map[string]any{"name": p.Name, "vendor": p.Vendor}
	if req.APIKey != nil {
		detail["key_rotated"] = true
	}
	h.record(r, audit.Success(audit.ActionProviderUpdate).Target("provider", p.ID).Detail(detail))
	writeJSON(w, http.StatusOK, map[string]any{"provider": d})
}

func (h *Handler) deleteSystemProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	p, err := h.providers.GetProvider(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if p.Scope != providerreg.ScopeSystem {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := h.providers.DeleteProvider(r.Context(), p.ID); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderDelete).Target("provider", p.ID).Detail(map[string]any{"name": p.Name}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setSystemDefaultProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	if err := h.providers.SetDefaultProvider(r.Context(), r.PathValue("pid")); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderSetDefault).Target("provider", r.PathValue("pid")))
	w.WriteHeader(http.StatusNoContent)
}

// ---- team tier ----

func (h *Handler) listTeamProviders(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	teamID := r.PathValue("id")
	providers, err := h.providers.VisibleToTeam(r.Context(), teamID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]providerDTO, 0, len(providers))
	for _, p := range providers {
		d, err := h.providerDTO(r.Context(), p)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		out = append(out, d)
	}
	assignment, err := h.providers.GetTeamAssignment(r.Context(), teamID)
	hasAssignment := err == nil
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": out,
		"assignment": func() any {
			if !hasAssignment {
				return nil
			}
			return map[string]any{"provider_id": assignment.ProviderID, "model_id": assignment.ModelID}
		}(),
	})
}

// requireTeamProvider guards a team-tier mutation on a team-owned provider:
// the provider must exist and belong to this team (system providers are not
// editable here). It returns the provider, or writes the error and returns nil.
func (h *Handler) requireTeamProvider(w http.ResponseWriter, r *http.Request) *providerreg.Provider {
	if !h.requireProviders(w) {
		return nil
	}
	p, err := h.providers.GetProvider(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeServiceError(w, err)
		return nil
	}
	if p.Scope != providerreg.ScopeTeam || p.TeamID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "provider not found")
		return nil
	}
	return &p
}

func (h *Handler) createTeamProvider(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	var req createProviderRequest
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if !validateVendor(w, req.Vendor) {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	p, err := h.providers.CreateProvider(r.Context(), providerreg.Provider{
		Scope:   providerreg.ScopeTeam,
		TeamID:  r.PathValue("id"),
		Name:    req.Name,
		Vendor:  req.Vendor,
		BaseURL: req.BaseURL,
		RawKey:  req.APIKey,
		Enabled: enabled,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	d, err := h.providerDTO(r.Context(), p)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderCreate).Target("provider", p.ID).Detail(map[string]any{"team_id": r.PathValue("id"), "name": p.Name, "vendor": p.Vendor}))
	writeJSON(w, http.StatusCreated, map[string]any{"provider": d})
}

func (h *Handler) updateTeamProvider(w http.ResponseWriter, r *http.Request) {
	p := h.requireTeamProvider(w, r)
	if p == nil {
		return
	}
	var req updateProviderRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Vendor != nil && !validateVendor(w, *req.Vendor) {
		return
	}
	upd, err := h.providers.UpdateProvider(r.Context(), p.ID, providerreg.ProviderUpdate{
		Name:    req.Name,
		Vendor:  req.Vendor,
		BaseURL: req.BaseURL,
		Key:     req.APIKey,
		Enabled: req.Enabled,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	d, err := h.providerDTO(r.Context(), upd)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	detail := map[string]any{"name": upd.Name, "vendor": upd.Vendor}
	if req.APIKey != nil {
		detail["key_rotated"] = true
	}
	h.record(r, audit.Success(audit.ActionProviderUpdate).Target("provider", upd.ID).Detail(detail))
	writeJSON(w, http.StatusOK, map[string]any{"provider": d})
}

func (h *Handler) deleteTeamProvider(w http.ResponseWriter, r *http.Request) {
	p := h.requireTeamProvider(w, r)
	if p == nil {
		return
	}
	if err := h.providers.DeleteProvider(r.Context(), p.ID); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderDelete).Target("provider", p.ID).Detail(map[string]any{"team_id": r.PathValue("id"), "name": p.Name}))
	w.WriteHeader(http.StatusNoContent)
}

// ---- models ----

// modelFetchDTO is one model returned by the fetch action: its provider-API
// name and whether the registry already holds it. The client picks which names
// to register — fetching never writes to the registry.
type modelFetchDTO struct {
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
}

func (h *Handler) fetchProviderModels(w http.ResponseWriter, r *http.Request, providerID string) {
	if !h.requireProviders(w) {
		return
	}
	p, err := h.providers.GetProvider(r.Context(), providerID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	names, err := providerreg.ListModels(r.Context(), providerreg.Target{
		Vendor:  p.Vendor,
		BaseURL: p.BaseURL,
		APIKey:  p.RawKey,
	}, nil, 0)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	existing := map[string]bool{}
	if ms, err := h.providers.ListModels(r.Context(), providerID); err == nil {
		for _, m := range ms {
			existing[m.Name] = true
		}
	}
	out := make([]modelFetchDTO, 0, len(names))
	for _, n := range names {
		out = append(out, modelFetchDTO{Name: n, Registered: existing[n]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (h *Handler) fetchSystemModels(w http.ResponseWriter, r *http.Request) {
	h.fetchProviderModels(w, r, r.PathValue("pid"))
}

func (h *Handler) fetchTeamModels(w http.ResponseWriter, r *http.Request) {
	p := h.requireTeamProvider(w, r)
	if p == nil {
		return
	}
	h.fetchProviderModels(w, r, p.ID)
}

// listProviderModels is shared by both tiers: the system route lists any
// provider's models, the team route lists a visible provider's models.
func (h *Handler) listProviderModels(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	models, err := h.providers.ListModels(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": modelDTOs(models)})
}

type createModelRequest struct {
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	Vision        bool   `json:"vision"`
	ContextWindow *int64 `json:"context_window"`
	Enabled       *bool  `json:"enabled"`
}

func (h *Handler) createModel(w http.ResponseWriter, r *http.Request, scope string) {
	var req createModelRequest
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "model name required")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	m, err := h.providers.CreateModel(r.Context(), providerreg.Model{
		ProviderID:    r.PathValue("pid"),
		Name:          req.Name,
		DisplayName:   req.DisplayName,
		Vision:        req.Vision,
		ContextWindow: req.ContextWindow,
		Enabled:       enabled,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderModelCreate).Target("model", m.ID).Detail(map[string]any{"provider_id": m.ProviderID, "name": m.Name, "scope": scope}))
	writeJSON(w, http.StatusCreated, map[string]any{"model": modelDTOs([]providerreg.Model{m})[0]})
}

func (h *Handler) createSystemModel(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	h.createModel(w, r, providerreg.ScopeSystem)
}

func (h *Handler) createTeamModel(w http.ResponseWriter, r *http.Request) {
	p := h.requireTeamProvider(w, r)
	if p == nil {
		return
	}
	h.createModel(w, r, providerreg.ScopeTeam)
}

type updateModelRequest struct {
	Name          *string `json:"name"`
	DisplayName   *string `json:"display_name"`
	Vision        *bool   `json:"vision"`
	ContextWindow *int64  `json:"context_window"`
	ClearContext  bool    `json:"clear_context_window"`
	Enabled       *bool   `json:"enabled"`
}

func (h *Handler) updateModel(w http.ResponseWriter, r *http.Request, scope string) {
	var req updateModelRequest
	if !decode(w, r, &req) {
		return
	}
	m, err := h.providers.UpdateModel(r.Context(), r.PathValue("mid"), providerreg.ModelUpdate{
		Name:          req.Name,
		DisplayName:   req.DisplayName,
		Vision:        req.Vision,
		ContextWindow: req.ContextWindow,
		ClearContext:  req.ClearContext,
		Enabled:       req.Enabled,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderModelUpdate).Target("model", m.ID).Detail(map[string]any{"provider_id": m.ProviderID, "name": m.Name, "scope": scope}))
	writeJSON(w, http.StatusOK, map[string]any{"model": modelDTOs([]providerreg.Model{m})[0]})
}

func (h *Handler) updateSystemModel(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	h.updateModel(w, r, providerreg.ScopeSystem)
}

func (h *Handler) updateTeamModel(w http.ResponseWriter, r *http.Request) {
	p := h.requireTeamProvider(w, r)
	if p == nil {
		return
	}
	h.updateModel(w, r, providerreg.ScopeTeam)
}

func (h *Handler) deleteModel(w http.ResponseWriter, r *http.Request, scope string) {
	mid := r.PathValue("mid")
	if err := h.providers.DeleteModel(r.Context(), mid); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderModelDelete).Target("model", mid).Detail(map[string]any{"provider_id": r.PathValue("pid"), "scope": scope}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteSystemModel(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	h.deleteModel(w, r, providerreg.ScopeSystem)
}

func (h *Handler) deleteTeamModel(w http.ResponseWriter, r *http.Request) {
	p := h.requireTeamProvider(w, r)
	if p == nil {
		return
	}
	h.deleteModel(w, r, providerreg.ScopeTeam)
}

func (h *Handler) setDefaultModel(w http.ResponseWriter, r *http.Request, scope string) {
	mid := r.PathValue("mid")
	if err := h.providers.SetDefaultModel(r.Context(), r.PathValue("pid"), mid); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionProviderModelDefault).Target("model", mid).Detail(map[string]any{"provider_id": r.PathValue("pid"), "scope": scope}))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setSystemDefaultModel(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	h.setDefaultModel(w, r, providerreg.ScopeSystem)
}

func (h *Handler) setTeamDefaultModel(w http.ResponseWriter, r *http.Request) {
	p := h.requireTeamProvider(w, r)
	if p == nil {
		return
	}
	h.setDefaultModel(w, r, providerreg.ScopeTeam)
}

// ---- team assignment ----

type setAssignmentRequest struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

func (h *Handler) setTeamAssignment(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	var req setAssignmentRequest
	if !decode(w, r, &req) {
		return
	}
	teamID := r.PathValue("id")
	if req.ProviderID == "" {
		writeError(w, http.StatusBadRequest, "provider_id required")
		return
	}
	// Scope guard: a team may only assign a provider it can see — a system
	// provider or one of its own. The store enforces enabled/model-belongs but
	// not visibility, so the guard lives here.
	p, err := h.providers.GetProvider(r.Context(), req.ProviderID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if p.Scope != providerreg.ScopeSystem && !(p.Scope == providerreg.ScopeTeam && p.TeamID == teamID) {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := h.providers.SetTeamAssignment(r.Context(), teamID, req.ProviderID, req.ModelID); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamProviderAssign).Target("team", teamID).Detail(map[string]any{"provider_id": req.ProviderID, "model_id": req.ModelID}))
	writeJSON(w, http.StatusOK, map[string]any{"assignment": map[string]any{"provider_id": req.ProviderID, "model_id": req.ModelID}})
}

func (h *Handler) clearTeamAssignment(w http.ResponseWriter, r *http.Request) {
	if !h.requireProviders(w) {
		return
	}
	teamID := r.PathValue("id")
	if err := h.providers.ClearTeamAssignment(r.Context(), teamID); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionTeamProviderAssignClear).Target("team", teamID))
	w.WriteHeader(http.StatusNoContent)
}
