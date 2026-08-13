package adminapi

import (
	"errors"
	"net/http"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/identity"
)

// TOTP self-service (MFA): enable/confirm/disable the account's authenticator
// second factor. Routes under /api/me/totp/**.

// enableTOTP: POST /api/me/totp/enable → {secret, uri} (shown once). The
// account is NOT protected until confirm succeeds.
func (h *Handler) enableTOTP(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	secret, uri, err := h.identity.EnrollTOTP(r.Context(), u.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMeTOTPEnroll))
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret, "uri": uri})
}

// confirmTOTP: POST /api/me/totp/confirm {code} — validates the pending
// secret and turns the second factor on.
func (h *Handler) confirmTOTP(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.identity.ConfirmTOTP(r.Context(), u.ID, req.Code); err != nil {
		if errors.Is(err, identity.ErrInvalidTOTP) {
			writeError(w, http.StatusBadRequest, "invalid verification code")
			return
		}
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMeTOTPConfirm))
	w.WriteHeader(http.StatusNoContent)
}

// disableTOTP: POST /api/me/totp/disable {code} — requires the current code.
func (h *Handler) disableTOTP(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.identity.DisableTOTP(r.Context(), u.ID, req.Code); err != nil {
		if errors.Is(err, identity.ErrInvalidTOTP) {
			writeError(w, http.StatusBadRequest, "invalid verification code")
			return
		}
		writeServiceError(w, err)
		return
	}
	h.record(r, audit.Success(audit.ActionMeTOTPDisable))
	w.WriteHeader(http.StatusNoContent)
}
