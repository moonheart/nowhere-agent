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
// by the server). system is the composed system prompt for this request
// (base + skills + recalled memory). Keeping it a factory lets the handler
// stay transport-only.
type LoopFactory func(ctx context.Context, system string) *agent.Loop

// ContextBuilder assembles the system prompt for a request: the base prompt
// plus the L0 skill index and recalled long-term memories for the caller's
// accessible scopes (design D5/D10 read side). It is the seam where memory
// recall and skill L0/L1 loading feed the agent loop (task 4.5).
type ContextBuilder interface {
	// SystemPrompt returns the composed system prompt for the user and query.
	SystemPrompt(ctx context.Context, user identity.User, query string) string
}

// Handler serves POST /api/chat.
type Handler struct {
	newLoop LoopFactory
	system  string
	// runtime, when set, persists each request as a session run: loop events
	// are teed into the durable run log (the episodes for dreaming) and the
	// single-active-run lock is enforced per session.
	runtime *session.Runtime
	// ctxBuilder, when set, composes the per-request system prompt (skills +
	// recalled memory). Nil keeps the static system prompt (tests).
	ctxBuilder ContextBuilder
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

// WithContextBuilder enables memory recall + skill L0 injection into the loop.
func (h *Handler) WithContextBuilder(cb ContextBuilder) *Handler {
	h.ctxBuilder = cb
	return h
}

// Register mounts the route.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat", h.serveChat)
	mux.HandleFunc("GET /api/chat/history", h.serveHistory)
	mux.HandleFunc("POST /api/chat/resume", h.serveResume)
	mux.HandleFunc("GET /api/chat/sessions", h.serveSessions)
	mux.HandleFunc("DELETE /api/chat/sessions/{id}", h.serveDeleteSession)
}

// RegisterAuthed mounts the route behind auth middleware, so each chat request
// resolves to an authenticated user (sessions are user-owned).
func (h *Handler) RegisterAuthed(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("POST /api/chat", auth(http.HandlerFunc(h.serveChat)))
	mux.Handle("GET /api/chat/history", auth(http.HandlerFunc(h.serveHistory)))
	mux.Handle("POST /api/chat/resume", auth(http.HandlerFunc(h.serveResume)))
	mux.Handle("GET /api/chat/sessions", auth(http.HandlerFunc(h.serveSessions)))
	mux.Handle("DELETE /api/chat/sessions/{id}", auth(http.HandlerFunc(h.serveDeleteSession)))
}

// sseEmitter adapts agent.Emitter to write ui-message-stream frames live.
type sseEmitter struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	mu          sync.Mutex
	msgID       string
	textID      string
	textStarted bool
	thinkID     string
	thinkOpen   bool
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

	emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1", thinkID: "reasoning-1"}
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})

	history := toHistory(req)
	loop := h.newLoop(r.Context(), h.systemPromptFor(r, req))

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

		// Tell the client which session it's talking to (transient: handled by
		// onData, not added to the message), so it can resume/replay later.
		emitter.write(chunk{"type": "data-session", "data": map[string]any{"id": sessID}, "transient": true})

		// Persist the user's message as the run's first event so history replay
		// reconstructs the user side, not just the assistant's output.
		if text := lastUserText(req); text != "" {
			payload, _ := json.Marshal(text)
			_ = h.runtime.AppendEvent(r.Context(), session.Event{
				RunID:     runID,
				SessionID: sessID,
				Kind:      string(agent.KindUser),
				Payload:   payload,
			})
		}

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

// systemPromptFor composes the system prompt for a request: the ContextBuilder
// (skills + recalled memory) when wired and the caller is authenticated,
// otherwise the static base prompt.
func (h *Handler) systemPromptFor(r *http.Request, req dataStreamRequest) string {
	if h.ctxBuilder == nil {
		return h.system
	}
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		return h.system
	}
	return h.ctxBuilder.SystemPrompt(r.Context(), user, lastUserText(req))
}

// resolveSession maps the request to a session: it resumes the session named
// by threadId when it exists and belongs to the caller, otherwise creates a
// new one for the caller.
func (h *Handler) resolveSession(r *http.Request, req dataStreamRequest) (session.Session, error) {
	userID := ""
	if u, ok := identity.UserFromContext(r.Context()); ok {
		userID = u.ID
	}
	if req.ThreadID != "" {
		if s, err := h.runtime.GetSession(r.Context(), req.ThreadID); err == nil && sessionVisibleTo(s, userID) {
			return s, nil
		}
		// Unknown or foreign thread id: fall through and create a fresh session.
	}
	return h.runtime.CreateSession(r.Context(), userID, lastUserText(req))
}

// sessionVisibleTo reports whether a caller may read/act on a session. A
// session is visible to its owner; when the caller is authenticated, only
// their own sessions qualify (a user-owned session is never shared cross-user).
func sessionVisibleTo(s session.Session, callerID string) bool {
	if callerID == "" {
		// Unauthenticated (tests/dev): only anonymous sessions are visible.
		return s.UserID == ""
	}
	return s.UserID == callerID
}

// authorizeSession resolves a session by id and checks the caller may access
// it, writing the appropriate HTTP error and returning ok=false otherwise.
func (h *Handler) authorizeSession(w http.ResponseWriter, r *http.Request, sessionID string) (session.Session, bool) {
	s, err := h.runtime.GetSession(r.Context(), sessionID)
	if err != nil {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return session.Session{}, false
	}
	callerID := ""
	if u, ok := identity.UserFromContext(r.Context()); ok {
		callerID = u.ID
	}
	if !sessionVisibleTo(s, callerID) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return session.Session{}, false
	}
	return s, true
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
	case agent.KindThinking:
		if !e.thinkOpen {
			e.write(chunk{"type": "reasoning-start", "id": e.thinkID})
			e.thinkOpen = true
		}
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "reasoning-delta", "delta": s})
		}
	case agent.KindText:
		if e.thinkOpen {
			e.write(chunk{"type": "reasoning-end"})
			e.thinkOpen = false
		}
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
	if e.thinkOpen {
		e.write(chunk{"type": "reasoning-end"})
		e.thinkOpen = false
	}
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
