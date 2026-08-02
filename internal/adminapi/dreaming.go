package adminapi

import (
	"errors"
	"net/http"
	"time"

	"nowhere-agent/internal/dreaming"
)

// Manual consolidation (memory-consolidation). Dreaming normally runs on a
// schedule; this lets an account consolidate its OWN sessions on demand —
// useful when the schedule is long, and when the scheduler is off entirely and
// consolidation is meant to be deliberate.
//
// The trigger is deliberately self-scoped. A pass reads conversations and
// spends provider tokens, so one account must never be able to start work over
// another's sessions.

// dreamStateDTO is what the console renders.
type dreamStateDTO struct {
	// Running is true while ANY pass is in flight, including the scheduled one
	// — it consolidates this caller's sessions too, so the button must be
	// disabled for it just the same.
	Running bool         `json:"running"`
	Mine    bool         `json:"mine"`
	Last    *dreamRunDTO `json:"last,omitempty"`
}

type dreamRunDTO struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Episodes   int       `json:"episodes"`
	Added      int       `json:"added"`
	Revised    int       `json:"revised"`
	Retired    int       `json:"retired"`
	Purged     int       `json:"purged"`
	Tokens     int       `json:"tokens"`
	// BudgetExhausted means some batches were deferred; their watermarks were
	// held, so a later pass picks them up. Surfaced because "nothing happened"
	// and "we ran out of budget" look identical otherwise.
	BudgetExhausted bool `json:"budget_exhausted"`
	// Compacted means the pass reviewed the existing store instead of learning
	// from new conversations, which is what happens when there are none.
	Compacted bool   `json:"compacted"`
	Error     string `json:"error,omitempty"`
}

func dreamStateOf(st dreaming.RunState) dreamStateDTO {
	out := dreamStateDTO{Running: st.Running, Mine: st.Mine}
	if st.Last != nil {
		r := st.Last
		out.Last = &dreamRunDTO{
			StartedAt:       r.StartedAt,
			FinishedAt:      r.FinishedAt,
			Episodes:        r.Result.EpisodesProcessed,
			Added:           r.Result.MemoriesWritten,
			Revised:         r.Result.MemoriesRevised,
			Retired:         r.Result.MemoriesRetired,
			Purged:          r.Result.MemoriesPurged,
			Tokens:          r.Result.TokensUsed,
			BudgetExhausted: r.Result.BudgetExhausted,
			Compacted:       r.Result.Compacted,
			Error:           r.Err,
		}
	}
	return out
}

func (h *Handler) dreamStatus(w http.ResponseWriter, r *http.Request) {
	if h.dreaming == nil {
		writeError(w, http.StatusServiceUnavailable, "consolidation unavailable")
		return
	}
	writeJSON(w, http.StatusOK, dreamStateOf(h.dreaming.Status(caller(r).ID)))
}

func (h *Handler) triggerDream(w http.ResponseWriter, r *http.Request) {
	if h.dreaming == nil {
		writeError(w, http.StatusServiceUnavailable, "consolidation unavailable")
		return
	}
	u := caller(r)
	if err := h.dreaming.TriggerForUser(u.ID); err != nil {
		if errors.Is(err, dreaming.ErrBusy) {
			// 409, not 429: this is not rate limiting, it is a single-flight
			// conflict the caller resolves by waiting for the running pass.
			writeError(w, http.StatusConflict, "a consolidation pass is already running")
			return
		}
		writeServiceError(w, err)
		return
	}
	// 202: the pass runs in the background and outlives this request. Returning
	// the state lets the console switch to "running" without a second call.
	writeJSON(w, http.StatusAccepted, dreamStateOf(h.dreaming.Status(u.ID)))
}
