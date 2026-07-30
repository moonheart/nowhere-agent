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
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	// Pick the run to resume: the in-flight one if any, else the latest.
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

// emitStreamEvent converts one live content frame (Kind + JSON payload) into
// the matching ui-message-stream frame, reusing the emitter's block framing.
// It is the live-delivery counterpart to emitResumeEvent: same render, but the
// source is the StreamBroker, not the durable run event log.
func emitStreamEvent(r *http.Request, emitter *sseEmitter, e session.StreamEvent) {
	// session_state is a live-only session-KV frame (capability-gap O1), not an
	// agent loop event: it is published directly to the broker by
	// Runtime.SetSessionStateKV when a state-producing tool (plan_write) writes.
	// Render it as a durable data frame so the client's plan panel updates live
	// and the frame lands in message metadata for a history reload.
	if e.Kind == "session_state" {
		if m, ok := decodeMapPayload(e.Payload); ok {
			emitter.write(chunk{"type": "data-session-state", "data": m})
		}
		return
	}
	switch agent.EventKind(e.Kind) {
	case agent.KindThinking, agent.KindText, agent.KindError:
		emitter.Emit(r.Context(), agent.EventKind(e.Kind), decodeTextPayload(e.Payload))
	case agent.KindToolUse, agent.KindToolResult:
		if m, ok := decodeMapPayload(e.Payload); ok {
			emitter.Emit(r.Context(), agent.EventKind(e.Kind), m)
		}
	case agent.KindUsage:
		// Token usage stashed for the finish frame; carried as a decoded object.
		if m, ok := decodeMapPayload(e.Payload); ok {
			emitter.Emit(r.Context(), agent.KindUsage, m)
		}
	case agent.KindSubagent:
		if m, ok := decodeMapPayload(e.Payload); ok {
			emitter.Emit(r.Context(), agent.KindSubagent, m)
		}
	case agent.KindInterrupt:
		// The client-interaction prompt (approval / ask_user / client-tool). It is
		// broker-routed content, so it arrives here on the content channel; decode
		// the Interaction payload and render the data-interaction frame.
		if m, ok := decodeMapPayload(e.Payload); ok {
			emitter.Emit(r.Context(), agent.KindInterrupt, m)
		}
	}
}

// emitLifecycleEvent renders a durable lifecycle event (running/done/cancelled)
// as a run-status frame so an attached client syncs run state. Lifecycle events
// carry no content payload.
func emitLifecycleEvent(r *http.Request, emitter *sseEmitter, e session.Event) {
	switch agent.EventKind(e.Kind) {
	case agent.KindRunning, agent.KindDone, agent.KindCancelled:
		emitter.Emit(r.Context(), agent.EventKind(e.Kind), nil)
	}
}
