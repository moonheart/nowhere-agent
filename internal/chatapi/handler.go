package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/session"
)

// LoopFactory builds an agent loop for a chat request (provider + tools wired
// by the server). Keeping it a factory lets the handler stay transport-only.
type LoopFactory func(ctx context.Context) *agent.Loop

// Handler serves POST /api/chat.
type Handler struct {
	newLoop LoopFactory
	system  string
	// runtime, when set, persists each request as a session run: loop events
	// are teed into the durable run log (the episodes for dreaming) and the
	// single-active-run lock is enforced per session.
	runtime *session.Runtime
}

// NewHandler creates a chat Handler.
func NewHandler(newLoop LoopFactory, systemPrompt string) *Handler {
	return &Handler{newLoop: newLoop, system: systemPrompt}
}

// WithRuntime enables run persistence for the chat endpoint.
func (h *Handler) WithRuntime(rt *session.Runtime) *Handler {
	h.runtime = rt
	return h
}

// Register mounts the route.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat", h.serveChat)
}

// RegisterAuthed mounts the route behind auth middleware, so each chat request
// resolves to an authenticated user (sessions are user-owned).
func (h *Handler) RegisterAuthed(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("POST /api/chat", auth(http.HandlerFunc(h.serveChat)))
}

// sseEmitter adapts agent.Emitter to write ui-message-stream frames live.
type sseEmitter struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	mu          sync.Mutex
	msgID       string
	textID      string
	textStarted bool
}

func (h *Handler) serveChat(w http.ResponseWriter, r *http.Request) {
	var req dataStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
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

	emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1"}
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})

	loop := h.newLoop(r.Context())
	history := toHistory(req)

	// When a runtime is wired, persist this request as a run within its session.
	var emit agent.Emitter = emitter
	var sessID, runID string
	if h.runtime != nil {
		s, err := h.resolveSession(r, req)
		if err != nil {
			emitter.Emit(r.Context(), agent.KindError, err.Error())
			emitter.finish()
			return
		}
		sessID = s.ID
		run, err := h.runtime.StartRun(r.Context(), sessID)
		if err != nil {
			emitter.Emit(r.Context(), agent.KindError, err.Error())
			emitter.finish()
			return
		}
		runID = run.ID
		emit = &persistEmitter{inner: emitter, rt: h.runtime, sessionID: sessID, runID: runID}
	}

	_, runErr := loop.Run(r.Context(), history, emit)
	if runErr != nil {
		emitter.Emit(r.Context(), agent.KindError, runErr.Error())
	}

	// Settle the run's terminal state.
	if h.runtime != nil && runID != "" {
		status := session.RunDone
		if runErr != nil {
			status = session.RunFailed
		}
		_ = h.runtime.CompleteRun(r.Context(), sessID, status)
	}

	emitter.finish()
}

// resolveSession maps the request to a session: it resumes the session named
// by threadId when it exists, otherwise creates a new one for the caller.
func (h *Handler) resolveSession(r *http.Request, req dataStreamRequest) (session.Session, error) {
	userID := ""
	if u, ok := identity.UserFromContext(r.Context()); ok {
		userID = u.ID
	}
	if req.ThreadID != "" {
		if s, err := h.runtime.GetSession(r.Context(), req.ThreadID); err == nil {
			return s, nil
		}
		// Unknown thread id: fall through and create a fresh session.
	}
	return h.runtime.CreateSession(r.Context(), userID, lastUserText(req))
}

// persistEmitter tees loop events to the SSE stream and the durable run log.
type persistEmitter struct {
	inner     agent.Emitter
	rt        *session.Runtime
	sessionID string
	runID     string
}

// Emit streams to the client first, then persists to the run log.
func (p *persistEmitter) Emit(ctx context.Context, kind agent.EventKind, payload any) error {
	err := p.inner.Emit(ctx, kind, payload)
	data, mErr := json.Marshal(payload)
	if mErr != nil {
		data = []byte("null")
	}
	_ = p.rt.AppendEvent(ctx, session.Event{
		RunID:     p.runID,
		SessionID: p.sessionID,
		Kind:      string(kind),
		Payload:   data,
	})
	return err
}

// Emit implements agent.Emitter, streaming frames as the loop produces them.
func (e *sseEmitter) Emit(_ context.Context, kind agent.EventKind, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch kind {
	case agent.KindText:
		if !e.textStarted {
			e.write(chunk{"type": "text-start", "id": e.textID})
			e.textStarted = true
		}
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "text-delta", "id": e.textID, "delta": s})
		}
	case agent.KindToolUse:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			e.write(chunk{"type": "tool-call-start", "toolCallId": id, "toolName": name})
			e.write(chunk{"type": "tool-call-end", "toolCallId": id})
		}
	case agent.KindToolResult:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["tool_use_id"].(string)
			isErr, _ := m["is_error"].(bool)
			e.write(chunk{"type": "tool-result", "toolCallId": id, "result": m["content"], "isError": isErr})
		}
	case agent.KindError:
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "error", "errorText": s})
		}
	}
	return nil
}

// finish closes the text block, sends finish + [DONE].
func (e *sseEmitter) finish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.textStarted {
		e.write(chunk{"type": "text-end", "id": e.textID})
	}
	e.write(chunk{
		"type":         "finish",
		"finishReason": "stop",
		"usage":        map[string]any{"inputTokens": 0, "outputTokens": 0},
	})
	e.writeRaw("data: [DONE]\n\n")
	e.flusher.Flush()
}

func (e *sseEmitter) write(c chunk) {
	e.writeRaw(sseFrame(c))
	e.flusher.Flush()
}

func (e *sseEmitter) writeRaw(s string) {
	_, _ = e.w.Write([]byte(s))
}
