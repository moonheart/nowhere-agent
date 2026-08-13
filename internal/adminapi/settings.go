package adminapi

import (
	"encoding/json"
	"net/http"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/mcp"
	"nowhere-agent/internal/settings"
)

// Runtime platform settings (no-restart configuration): the admin console
// lists every known key (grouped into tabs, typed, help text attached) with
// its current effective value (env default or a persisted override) and
// writes overrides; clearing a key (JSON null) returns it to the env default.
// Changes apply on the next use.

// settingValue is the wire shape of one key: the effective value, its tab,
// type, secrecy, and the description the console renders.
type settingValue struct {
	Key         string          `json:"key"`
	Group       settings.Group  `json:"group"`
	Kind        settings.Kind   `json:"kind"`
	Value       json.RawMessage `json:"value"`
	// Secret marks values that are never echoed back (value is always null).
	Secret      bool            `json:"secret"`
	Description string          `json:"description"`
}

// listSettings: GET /api/admin/settings — every known key with its current
// effective value, grouped for the console's tabs.
func (h *Handler) listSettings(w http.ResponseWriter, r *http.Request) {
	if h.runtimeSettings == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime settings not enabled")
		return
	}
	out := make([]settingValue, 0, len(settings.Catalog()))
	for _, info := range settings.Catalog() {
		var value json.RawMessage
		if info.Secret {
			// Never echo a secret back: the console renders "set/unset".
			value = json.RawMessage("null")
		} else {
			v, err := json.Marshal(effectiveValue(h.runtimeSettings, info.Key))
			if err != nil {
				writeServiceError(w, err)
				return
			}
			value = v
		}
		out = append(out, settingValue{
			Key:         info.Key,
			Group:       info.Group,
			Kind:        info.Kind,
			Value:       value,
			Secret:      info.Secret,
			Description: info.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": out})
}

// effectiveValue renders the typed current value for the wire (strings, ints,
// floats, bools; unknown keys render null).
func effectiveValue(s SettingStore, key string) any {
	switch settings.Info(key).Kind {
	case settings.KindInt:
		return s.Int(key)
	case settings.KindFloat:
		return s.Float64(key)
	case settings.KindBool:
		return s.Bool(key)
	case settings.KindString:
		return s.String(key)
	}
	return nil
}

// putSetting: PUT /api/admin/settings/{key} {value} — writes an override
// (JSON null clears it, returning to the env default). The key must be known
// and the value must match the key's kind and any enum constraint.
func (h *Handler) putSetting(w http.ResponseWriter, r *http.Request) {
	if h.runtimeSettings == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime settings not enabled")
		return
	}
	key := r.PathValue("key")
	info := settings.Info(key)
	if info.Key == "" {
		writeError(w, http.StatusNotFound, "unknown setting key")
		return
	}
	var req struct {
		Value json.RawMessage `json:"value"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Value) == 0 {
		writeError(w, http.StatusBadRequest, "value is required (null clears the override)")
		return
	}
	if err := validateSettingValue(info, req.Value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.runtimeSettings.Set(r.Context(), key, req.Value); err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionSettingUpdate).Target("setting", key).Detail(map[string]any{"cleared": string(req.Value) == "null"}))
	w.WriteHeader(http.StatusNoContent)
}

// validateSettingValue checks a PUT payload against the key's kind and
// enum/list constraints. Enums are rejected early (a console typo must not
// silently flip a permission decision to the ask default); free-form strings
// are validated lazily by the read paths, which log and fail the tool/run
// rather than the whole server.
func validateSettingValue(info settings.KeyInfo, raw json.RawMessage) error {
	switch info.Kind {
	case settings.KindInt:
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return errInvalidSetting("must be an integer")
		}
		if n < 0 {
			return errInvalidSetting("must be >= 0")
		}
	case settings.KindFloat:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return errInvalidSetting("must be a number")
		}
	case settings.KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return errInvalidSetting("must be true or false")
		}
	case settings.KindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return errInvalidSetting("must be a string")
		}
		if !allowedSettingString(info.Key, s) {
			return errInvalidSetting("invalid value; allowed: " + allowedSettingValues(info.Key))
		}
		if info.Key == settings.KeyMCPServers {
			if err := validateMCPServers(s); err != nil {
				return err
			}
		}
	}
	return nil
}

// allowedSettingString reports whether s is valid for an enum-constrained key.
func allowedSettingString(key, s string) bool {
	switch key {
	case settings.KeySystemLang:
		return s == "en" || s == "zh"
	case settings.KeySandboxNetwork:
		return s == "deny" || s == "open" || s == "allowlist"
	case settings.KeyRedactStrategy:
		return s == "redact" || s == "mask"
	case settings.KeyPermissionReadOnly, settings.KeyPermissionSandboxWrite,
		settings.KeyPermissionNetwork, settings.KeyPermissionExternalWrite:
		return s == "allow" || s == "ask" || s == "deny"
	}
	return true
}

// validateMCPServers checks the MCP_SERVERS JSON shape early so a console typo
// is rejected with a 400 instead of silently keeping the previous server set.
func validateMCPServers(s string) error {
	if _, err := mcp.ParseServers(s); err != nil {
		return errInvalidSetting(err.Error())
	}
	return nil
}

// allowedSettingValues returns the human-readable enum for error messages.
func allowedSettingValues(key string) string {
	switch key {
	case settings.KeySystemLang:
		return "en, zh"
	case settings.KeySandboxNetwork:
		return "deny, open, allowlist"
	case settings.KeyRedactStrategy:
		return "redact, mask"
	case settings.KeyPermissionReadOnly, settings.KeyPermissionSandboxWrite,
		settings.KeyPermissionNetwork, settings.KeyPermissionExternalWrite:
		return "allow, ask, deny"
	}
	return "any string"
}

// errInvalidSetting builds the 400 payload for a rejected setting value.
func errInvalidSetting(reason string) error {
	return &invalidSettingError{reason}
}

type invalidSettingError struct{ reason string }

func (e *invalidSettingError) Error() string { return "invalid setting value: " + e.reason }
