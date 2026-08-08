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

// runNow fires one task immediately, out of band. Ownership is re-verified so a
// caller can only run their own task. The manual fire does not claim the task,
// so next_run_at/cron are untouched; a busy target under reject/enqueue is a
// quiet skip (started=false). Without a runner wired (no LLM) it answers 503.
func (h *Handler) runNow(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	if h.runner == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduled firing unavailable: no LLM provider configured")
		return
	}
	t, ok := h.authorizeTask(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := h.runner.FireNow(r.Context(), t); err != nil {
		writeStoreError(w, err)
		return
	}
	// The run may already have appended to its target session; report the task's
	// current produced sessions so the client can surface the newest one.
	ids, err := h.store.ListSessions(r.Context(), t.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	started := t.TargetSessionID != "" || len(ids) > 0
	var sessionID string
	if t.TargetSessionID != "" {
		sessionID = t.TargetSessionID
	} else if len(ids) > 0 {
		sessionID = ids[0]
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": started, "session_id": sessionID})
}

// sessions lists the sessions a task produced, with the title and created time
// the console renders. Each entry links into the chat view by id.
func (h *Handler) sessions(w http.ResponseWriter, r *http.Request) {
	if h.storeUnavailable(w) {
		return
	}
	if _, ok := h.authorizeTask(w, r, r.PathValue("id")); !ok {
		return
	}
	infos, err := h.store.ListProducedSessions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": infos})
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
