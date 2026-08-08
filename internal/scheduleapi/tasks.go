package scheduleapi

import (
	"net/http"
)

// Self-service routes (/api/me/scheduled-tasks/**). They need no tier guard
// beyond authentication: each confines itself to the authenticated caller's own
// tasks, so there is nothing further to authorize on create/list. Single-task
// operations re-verify ownership via authorizeTask.

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	ts, err := h.store.ListForUser(r.Context(), caller(r).ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": taskDTOs(ts)})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	var req taskRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := req.toTask(caller(r).ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	saved, err := h.store.Create(r.Context(), t)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"task": taskDTOOf(saved)})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	t, ok := h.authorizeTask(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskDTOOf(t)})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	// Resolve first: the update applies to THIS task, so it must already exist
	// and be the caller's (a PUT to a foreign or missing id is not an upsert).
	existing, ok := h.authorizeTask(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req taskRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := req.toTask(caller(r).ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	t.ID = existing.ID
	t.Enabled = existing.Enabled // an update does not flip the enable gate
	saved, err := h.store.Update(r.Context(), t)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskDTOOf(saved)})
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeTask(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := h.store.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) enable(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, true)
}

func (h *Handler) disable(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, false)
}

func (h *Handler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeTask(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := h.store.SetEnabled(r.Context(), r.PathValue("id"), enabled); err != nil {
		writeStoreError(w, err)
		return
	}
	t, err := h.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskDTOOf(t)})
}

// sessions lists the sessions a task produced.
func (h *Handler) sessions(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeTask(w, r, r.PathValue("id")); !ok {
		return
	}
	ids, err := h.store.ListSessions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": ids})
}

// clearSessions soft-deletes every session a task produced, returning how many
// were cleared. Ownership is re-verified so a caller can never clear another
// user's runs. Soft-delete keeps the rows for audit; the list simply empties.
func (h *Handler) clearSessions(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeTask(w, r, r.PathValue("id")); !ok {
		return
	}
	cleared, err := h.store.EndSessions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}
