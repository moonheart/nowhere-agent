package chatapi

import (
	"net/http"
	"strconv"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/session"
)

// serveResume handles POST /api/chat/resume?threadId=<id>&after=<offset>: it
// streams a session's in-progress (or last) run back to a reconnecting client.
// It shares the single attach path with serveChat (D3): subscribe → replay the
// gap → live-follow. The body uses the same ui-message-stream frames as
// /api/chat so the client's history.resume() decodes it with the same pipeline.
func (h *Handler) serveResume(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		http.Error(w, `{"error":"resume unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	threadID := r.URL.Query().Get("threadId")
	if threadID == "" {
		http.Error(w, `{"error":"threadId required"}`, http.StatusBadRequest)
		return
	}
	if _, ok := h.authorizeSession(w, r, threadID); !ok {
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))

	// Pick the run to resume: the active one if any, else the latest.
	run, ok, err := h.runtime.ActiveRun(r.Context(), threadID)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	if !ok {
		run, ok = h.latestRun(r, threadID)
		if !ok {
			http.Error(w, `{"error":"no run to resume"}`, http.StatusNotFound)
			return
		}
	}

	// An in-flight run is always re-streamed from the start. The attaching
	// client's follow renders ONE assistant message built from this whole stream;
	// honouring a non-zero `after` for an active run would split the run into a
	// snapshot bubble plus a continuation bubble, because the runtime's resume
	// creates a NEW assistant message rather than extending the loaded one. The
	// `after` offset is only meaningful for a settled run, where it bounds replay.
	if !run.Status.Terminal() {
		after = 0
	}

	if !writeStreamHeaders(w) {
		return
	}
	h.attach(w, r, threadID, run, after, nil)
}

// latestRun returns the most recent run in the session regardless of state.
func (h *Handler) latestRun(r *http.Request, sessionID string) (session.Run, bool) {
	runs, err := h.runtime.RunsForSession(r.Context(), sessionID)
	if err != nil || len(runs) == 0 {
		return session.Run{}, false
	}
	latest := runs[0]
	for _, run := range runs[1:] {
		if run.Seq > latest.Seq {
			latest = run
		}
	}
	return latest, true
}

// emitResumeEvent converts one persisted run event (Kind + JSON payload) into
// the matching ui-message-stream frame, reusing the emitter's block framing.
func emitResumeEvent(r *http.Request, emitter *sseEmitter, e session.Event) {
	switch agent.EventKind(e.Kind) {
	case agent.KindRunning, agent.KindDone:
		// Lifecycle events: no content payload, just the run-status frame so an
		// attached client syncs run state (start / natural finish).
		emitter.Emit(r.Context(), agent.EventKind(e.Kind), nil)
	case agent.KindThinking, agent.KindText, agent.KindError:
		emitter.Emit(r.Context(), agent.EventKind(e.Kind), decodeTextPayload(e.Payload))
	case agent.KindCancelled:
		emitter.Emit(r.Context(), agent.KindCancelled, nil)
	case agent.KindToolUse, agent.KindToolResult:
		if m, ok := decodeMapPayload(e.Payload); ok {
			emitter.Emit(r.Context(), agent.EventKind(e.Kind), m)
		}
	}
	// KindUser is not assistant output; the client already has the user message
	// and finish() closes the message.
}
