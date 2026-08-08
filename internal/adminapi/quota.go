package adminapi

import (
	"net/http"
	"strings"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/quota"
)

// Quota-configuration routes (enterprise-readiness P1-1 management face).
// A budget is the lever that makes usage accounting bite: without one there is
// no limit, so these routes set and clear the monthly token caps the quota
// checker enforces at run submit. All sit behind requireAdmin — a budget
// throttles someone else's spend, so it is a platform-administration act.
//
// Budgets are keyed by (scope, owner_id): scope "user" caps one account, scope
// "team" caps the spend billed to one team's provider key.

// quotaDTO is the wire shape of one budget. It mirrors quota.Budget.
type quotaDTO struct {
	Scope         string `json:"scope"`
	OwnerID       string `json:"owner_id"`
	MonthlyTokens int64  `json:"monthly_tokens"`
	UpdatedAt     string `json:"updated_at"`
}

func quotaDTOOf(b quota.Budget) quotaDTO {
	return quotaDTO{
		Scope:         string(b.Scope),
		OwnerID:       b.OwnerID,
		MonthlyTokens: b.MonthlyTokens,
		UpdatedAt:     b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// parseScope validates the scope query/body value, answering the response
// itself on a bad one.
func parseScope(w http.ResponseWriter, raw string) (quota.Scope, bool) {
	switch quota.Scope(raw) {
	case quota.ScopeUser:
		return quota.ScopeUser, true
	case quota.ScopeTeam:
		return quota.ScopeTeam, true
	default:
		writeError(w, http.StatusBadRequest, "scope must be user or team")
		return "", false
	}
}

// listQuotas reads one scope's budget: GET /api/admin/quotas?scope=&owner_id=.
// A budget is meaningful only against its owner, so both are required — there
// is no "list every budget" surface, because the point of a budget is the one
// account or team it throttles, not an inventory.
func (h *Handler) listQuotas(w http.ResponseWriter, r *http.Request) {
	if h.quotas == nil {
		writeError(w, http.StatusServiceUnavailable, "quota configuration unavailable")
		return
	}
	q := r.URL.Query()
	scope, ok := parseScope(w, q.Get("scope"))
	if !ok {
		return
	}
	ownerID := strings.TrimSpace(q.Get("owner_id"))
	if ownerID == "" {
		writeError(w, http.StatusBadRequest, "owner_id required")
		return
	}
	b, found, err := h.quotas.Get(r.Context(), scope, ownerID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !found {
		// No budget is a real, meaningful state ("no limit") — answer 200 with
		// budget:null rather than 404, so the console renders "no cap" instead of
		// an error for the common case of an unbudgeted scope.
		writeJSON(w, http.StatusOK, map[string]any{"budget": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"budget": quotaDTOOf(b)})
}

type putQuotaRequest struct {
	Scope         string `json:"scope"`
	OwnerID       string `json:"owner_id"`
	MonthlyTokens int64  `json:"monthly_tokens"`
}

// putQuota upserts one scope's monthly budget: PUT /api/admin/quotas. The store
// rejects a non-positive limit (block-everything is never the intent); clearing
// is the explicit way to remove a cap.
func (h *Handler) putQuota(w http.ResponseWriter, r *http.Request) {
	if h.quotas == nil {
		writeError(w, http.StatusServiceUnavailable, "quota configuration unavailable")
		return
	}
	var req putQuotaRequest
	if !decode(w, r, &req) {
		return
	}
	scope, ok := parseScope(w, req.Scope)
	if !ok {
		return
	}
	ownerID := strings.TrimSpace(req.OwnerID)
	if ownerID == "" {
		writeError(w, http.StatusBadRequest, "owner_id required")
		return
	}
	if req.MonthlyTokens <= 0 {
		writeError(w, http.StatusBadRequest, "monthly_tokens must be positive")
		return
	}
	if err := h.quotas.Set(r.Context(), scope, ownerID, req.MonthlyTokens); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionQuotaSet).
		Target(string(scope), ownerID).
		Detail(map[string]any{"monthly_tokens": req.MonthlyTokens}))

	b, _, err := h.quotas.Get(r.Context(), scope, ownerID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"budget": quotaDTOOf(b)})
}

// clearQuota removes one scope's budget, restoring "no limit":
// DELETE /api/admin/quotas?scope=&owner_id=.
func (h *Handler) clearQuota(w http.ResponseWriter, r *http.Request) {
	if h.quotas == nil {
		writeError(w, http.StatusServiceUnavailable, "quota configuration unavailable")
		return
	}
	q := r.URL.Query()
	scope, ok := parseScope(w, q.Get("scope"))
	if !ok {
		return
	}
	ownerID := strings.TrimSpace(q.Get("owner_id"))
	if ownerID == "" {
		writeError(w, http.StatusBadRequest, "owner_id required")
		return
	}
	cleared, err := h.quotas.Clear(r.Context(), scope, ownerID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !cleared {
		writeError(w, http.StatusNotFound, "no budget set for that scope and owner")
		return
	}
	h.record(r, audit.Success(audit.ActionQuotaClear).Target(string(scope), ownerID))
	w.WriteHeader(http.StatusNoContent)
}
