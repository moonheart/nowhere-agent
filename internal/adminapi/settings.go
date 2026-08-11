package adminapi

import (
	"encoding/json"
	"net/http"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/settings"
)

// Runtime platform settings (no-restart configuration): the admin console
// lists every known key with its current effective value (env default or a
// persisted override) and writes overrides; clearing a key (JSON null)
// returns it to the env default. Changes apply on the next use.

// settingValue is the wire shape of one key: the effective value plus the
// description the console renders.
type settingValue struct {
	Key         string          `json:"key"`
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
}

// settingDescriptions are the console-facing help strings.
var settingDescriptions = map[string]string{
	settings.KeyHTTPToolAllowlist: "Comma-separated http_request host allowlist (same syntax as HTTP_TOOL_ALLOWLIST). Empty disables the tool.",
	settings.KeyQueryDBDsns:       "Comma-separated name=dsn list for query_db (same syntax as QUERY_DB_DSNS). Empty disables the tool.",
	settings.KeyWebhookURL:        "Global run-completion notification target (overrides WEBHOOK_URL; task and inbound-webhook URLs still win per run).",
	settings.KeySystemLang:        "System-prompt language for new runs: en | zh (overrides LLM_SYSTEM_LANG).",
	settings.KeyRateLimitRPS:      "Per-IP HTTP rate limit, requests per second (0 = disabled; overrides HTTP_RATE_LIMIT_RPS).",
	settings.KeyRateLimitBurst:    "Per-IP HTTP rate limit burst size (0 = disabled; overrides HTTP_RATE_LIMIT_BURST).",
}

// listSettings: GET /api/admin/settings — every known key with its current
// effective value.
func (h *Handler) listSettings(w http.ResponseWriter, r *http.Request) {
	if h.runtimeSettings == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime settings not enabled")
		return
	}
	out := make([]settingValue, 0, len(settings.Keys()))
	for _, key := range settings.Keys() {
		value, err := json.Marshal(effectiveValue(h.runtimeSettings, key))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		out = append(out, settingValue{
			Key:         key,
			Value:       value,
			Description: settingDescriptions[key],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": out})
}

// effectiveValue renders the typed current value for the wire (strings and
// ints; unknown keys render null).
func effectiveValue(s SettingStore, key string) any {
	switch key {
	case settings.KeyHTTPToolAllowlist, settings.KeyQueryDBDsns,
		settings.KeyWebhookURL, settings.KeySystemLang:
		return s.String(key)
	case settings.KeyRateLimitRPS, settings.KeyRateLimitBurst:
		return s.Int(key)
	}
	return nil
}

// putSetting: PUT /api/admin/settings/{key} {value} — writes an override
// (JSON null clears it, returning to the env default). The key must be known.
func (h *Handler) putSetting(w http.ResponseWriter, r *http.Request) {
	if h.runtimeSettings == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime settings not enabled")
		return
	}
	key := r.PathValue("key")
	if !knownSetting(key) {
		writeError(w, http.StatusNotFound, "unknown setting key")
		return
	}
	var req struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(req.Value) == 0 {
		writeError(w, http.StatusBadRequest, "value is required (null clears the override)")
		return
	}
	if err := h.runtimeSettings.Set(r.Context(), key, req.Value); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionSettingUpdate).Target("setting", key).Detail(map[string]any{"cleared": string(req.Value) == "null"}))
	w.WriteHeader(http.StatusNoContent)
}

// knownSetting reports whether key is a runtime setting the console may edit.
func knownSetting(key string) bool {
	for _, k := range settings.Keys() {
		if k == key {
			return true
		}
	}
	return false
}
