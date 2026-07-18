package chatapi

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/session"
)

// serveResume handles POST /api/chat/resume?threadId=<id>&after=<offset>: it
// streams a session's in-progress (or last) run back to a reconnecting client.
// It subscribes BEFORE replaying so no event falls into the gap between the
// two, then replays the persisted gap and live-follows new events. The body
// uses the same ui-message-stream frames as /api/chat so the client's
// history.resume() can decode it with the identical pipeline.
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

	// SSE headers for ui-message-stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("x-vercel-ai-ui-message-stream", "v1")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1", thinkID: "reasoning-1"}
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})

	// Subscribe first: any event appended from now on is buffered on ch, so the
	// replay below cannot race past a live event and lose it.
	ch, unsub := h.runtime.Subscribe(threadID, 128)
	defer unsub()

	// Replay the persisted gap (events up to and including whatever landed
	// before the subscribe are also on ch; the offset filter dedups them).
	replayed, err := h.runtime.Replay(r.Context(), run.ID, after)
	if err != nil {
		emitter.Emit(r.Context(), agent.KindError, err.Error())
		emitter.finish()
		return
	}
	maxOffset := after
	for _, e := range replayed {
		emitResumeEvent(r, emitter, e)
		if e.Offset > maxOffset {
			maxOffset = e.Offset
		}
	}

	// If the run already settled, replay alone is the whole answer.
	if run.Status.Terminal() {
		emitter.finish()
		return
	}

	// Live-follow buffered + new events until the run settles or the client
	// disconnects. After each event we re-check the run state so a run that
	// ended in the subscribe/replay window still terminates the stream.
	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-ch:
			if !open {
				emitter.finish()
				return
			}
			if e.RunID != run.ID || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitResumeEvent(r, emitter, e)
			if agent.EventKind(e.Kind) == agent.KindDone || agent.EventKind(e.Kind) == agent.KindError {
				emitter.finish()
				return
			}
			// The run may have settled without a further event we can observe
			// (e.g. its terminal event was dropped as a slow-consumer). Bail out
			// once it is no longer active rather than block forever.
			if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), threadID); !stillActive {
				emitter.finish()
				return
			}
		}
	}
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
	case agent.KindThinking, agent.KindText, agent.KindError:
		emitter.Emit(r.Context(), agent.EventKind(e.Kind), decodeTextPayload(e.Payload))
	case agent.KindToolUse, agent.KindToolResult:
		if m, ok := decodeMapPayload(e.Payload); ok {
			emitter.Emit(r.Context(), agent.EventKind(e.Kind), m)
		}
	}
	// KindUser / KindDone are not assistant output; the client already has the
	// user message and finish() closes the message.
}
