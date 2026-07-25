package chatapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/workspace"
)

// LoopFactory builds an agent loop for a chat request (provider + tools wired
// by the server). system is the composed system prompt for this request
// (base + skills + recalled memory). Keeping it a factory lets the handler
// stay transport-only.
type LoopFactory func(ctx context.Context, system string) *agent.Loop

// ToolBinder attaches session-scoped tools to a loop once the session id is
// known (file-tools D6). The server implements it by ensuring the session's
// sandbox and registering the file tools bound to it. Nil disables tools.
type ToolBinder func(ctx context.Context, loop *agent.Loop, sessionID string)

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
	// registry, when set, executes runs on connection-independent worker
	// goroutines (design decouple-run-ownership). serveChat submits to it and
	// attaches; cancel is transport-independent.
	registry *session.RunRegistry
	// ctxBuilder, when set, composes the per-request system prompt (skills +
	// recalled memory). Nil keeps the static system prompt (tests).
	ctxBuilder ContextBuilder
	// images, when set, serves workspace image files to the session owner via
	// GET /api/chat/sessions/{id}/files/... (persist-raw-messages D6).
	images *workspace.ImageStore
	// msgStore, when set, is the authoritative conversation record: serveChat
	// rebuilds cross-run history from it (ignoring client-sent history) and the
	// run registry persists assembled messages into it.
	msgStore session.MessageStore
	// bindTools, when set, attaches session-scoped tools (file tools bound to
	// the session's sandbox) to each loop after the session is resolved.
	bindTools ToolBinder
}

// NewHandler creates a chat Handler.
func NewHandler(newLoop LoopFactory, systemPrompt string) *Handler {
	return &Handler{newLoop: newLoop, system: systemPrompt}
}

// WithRuntime enables run persistence for the chat endpoint. It also wires a
// RunRegistry over the same runtime and bus, so runs execute on connection-
// independent workers and every client (submitter or attacher) shares the one
// attach path. Pass a custom registry via WithRegistry to override.
func (h *Handler) WithRuntime(rt *session.Runtime) *Handler {
	h.runtime = rt
	h.registry = session.NewRunRegistry(rt, rt.Bus())
	return h
}

// WithRegistry overrides the run-execution registry (default: one built over the
// runtime in WithRuntime).
func (h *Handler) WithRegistry(rg *session.RunRegistry) *Handler {
	h.registry = rg
	return h
}

// WithMessageStore wires full-block message persistence into the run-execution
// registry and authoritative history rebuild (persist-raw-messages). Call after
// WithRuntime/WithRegistry.
func (h *Handler) WithMessageStore(ms session.MessageStore) *Handler {
	h.msgStore = ms
	if h.registry != nil {
		h.registry.WithMessageStore(ms)
	}
	return h
}

// WithImageStore wires workspace image serving (GET .../files/...) for the
// session owner. Call after WithRuntime.
func (h *Handler) WithImageStore(is *workspace.ImageStore) *Handler {
	h.images = is
	return h
}

// WithContextBuilder enables memory recall + skill L0 injection into the loop.
func (h *Handler) WithContextBuilder(cb ContextBuilder) *Handler {
	h.ctxBuilder = cb
	return h
}

// WithToolBinder enables per-session tool wiring (file-tools): the binder runs
// for each run after the session is resolved, attaching the session's
// sandbox-bound tools to the loop.
func (h *Handler) WithToolBinder(b ToolBinder) *Handler {
	h.bindTools = b
	return h
}

// Register mounts the route.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat", h.serveChat)
	mux.HandleFunc("GET /api/chat/history", h.serveHistory)
	mux.HandleFunc("POST /api/chat/resume", h.serveResume)
	mux.HandleFunc("POST /api/chat/cancel", h.serveCancel)
	mux.HandleFunc("GET /api/chat/sessions", h.serveSessions)
	mux.HandleFunc("DELETE /api/chat/sessions/{id}", h.serveDeleteSession)
	mux.HandleFunc("GET /api/chat/sessions/{id}/files/{path...}", h.serveFile)
}

// RegisterAuthed mounts the route behind auth middleware, so each chat request
// resolves to an authenticated user (sessions are user-owned).
func (h *Handler) RegisterAuthed(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("POST /api/chat", auth(http.HandlerFunc(h.serveChat)))
	mux.Handle("GET /api/chat/history", auth(http.HandlerFunc(h.serveHistory)))
	mux.Handle("POST /api/chat/resume", auth(http.HandlerFunc(h.serveResume)))
	mux.Handle("POST /api/chat/cancel", auth(http.HandlerFunc(h.serveCancel)))
	mux.Handle("GET /api/chat/sessions", auth(http.HandlerFunc(h.serveSessions)))
	mux.Handle("DELETE /api/chat/sessions/{id}", auth(http.HandlerFunc(h.serveDeleteSession)))
	mux.Handle("GET /api/chat/sessions/{id}/files/{path...}", auth(http.HandlerFunc(h.serveFile)))
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
	// writeErr latches the first failed write (e.g. client disconnected), so
	// subsequent Emits report it and the loop unwinds instead of writing into
	// the void while the run leaks.
	writeErr error
	// usageIn/usageOut hold the run's reported token usage, stashed from a
	// KindUsage event, so finish() reports real counts instead of zeros.
	usageIn  int
	usageOut int
}

func (h *Handler) serveChat(w http.ResponseWriter, r *http.Request) {
	var req dataStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	history := toHistory(req)
	loop := h.newLoop(r.Context(), h.systemPromptFor(r, req))

	// No runtime wired (tests/dev): stream the loop directly with no persistence,
	// no registry, no run-state — the pre-registry behaviour.
	if h.runtime == nil || h.registry == nil {
		h.serveChatDirect(w, r, loop, history)
		return
	}

	// Resolve the session and submit the run to the registry, which executes it
	// on a connection-independent worker goroutine. Then this handler simply
	// attaches to the run's event stream — the identical path serveResume uses —
	// so the submitter and every attacher are symmetric consumers (D3).
	s, err := h.resolveSession(r, req)
	if err != nil {
		writeSSEError(w, err.Error())
		return
	}
	sessID := s.ID

	// Bind the image materializer to this session so any BlockImage in the
	// rebuilt history or produced output resolves within this session's
	// workspace (confined). Set before the run starts.
	if h.images != nil {
		loop.WithImages(h.images.ResolverFor(sessID))
	}

	// Attach this session's sandbox-bound tools (file-tools) now that the
	// session id is known. The binder ensures the session's sandbox and
	// registers its file tools into the loop's registry.
	if h.bindTools != nil {
		h.bindTools(r.Context(), loop, sessID)
	}

	// Build the user turn's message so the run worker can persist it (full-block
	// conversation record) in addition to the replay event below.
	var userMsg *provider.Message
	if text := lastUserText(req); text != "" {
		m := provider.TextMessage(provider.RoleUser, text)
		userMsg = &m
	}

	// Authoritative history (persist-raw-messages): when this request resumes an
	// existing session and a MessageStore is wired, rebuild the conversation from
	// the durable record — with full blocks (thinking+signature, tool_use,
	// tool_result) — instead of trusting the client-sent messages, which are
	// text-only and forgeable. The new user turn is appended so the loop sees the
	// complete conversation. For a fresh session (or no store) the client history
	// is all there is, so fall back to it.
	if h.msgStore != nil && req.ThreadID != "" && s.ID == req.ThreadID {
		if stored, err := h.msgStore.MessagesFor(r.Context(), sessID); err == nil && len(stored) > 0 {
			history = storedMessagesToProvider(stored)
			if userMsg != nil {
				history = append(history, *userMsg)
			}
		}
	}

	run, err := h.registry.Submit(r.Context(), sessID, session.RunWork{Loop: loop, History: history, UserMessage: userMsg})
	if err != nil {
		// Single-active-run: a second client submitting while a run is in flight
		// is rejected (multi-writer prevention), not queued. Checked before any
		// SSE headers are written so the status isn't locked to 200.
		if errors.Is(err, session.ErrRunActive) {
			http.Error(w, `{"error":"a run is already active in this session"}`, http.StatusConflict)
			return
		}
		writeSSEError(w, err.Error())
		return
	}

	// The run's user turn is persisted by the registry at run start (before the
	// worker launches), so history replay reconstructs the user side and the
	// event deterministically precedes any run output.

	// SSE headers for ui-message-stream.
	if !writeStreamHeaders(w) {
		return
	}
	// Tell the client which session it's talking to (transient: handled by onData,
	// not added to the message), so it can resume/replay later.
	pre := []chunk{{"type": "data-session", "data": map[string]any{"id": sessID}, "transient": true}}

	// Attach to the run from the start; the worker is already executing.
	h.attach(w, r, sessID, run, 0, pre)
}

// serveChatDirect streams a loop's output with no run persistence (no runtime
// wired — tests/dev). The loop runs on the request goroutine and is cancelled
// when the client disconnects, exactly as before the registry existed.
func (h *Handler) serveChatDirect(w http.ResponseWriter, r *http.Request, loop *agent.Loop, history []provider.Message) {
	if !writeStreamHeaders(w) {
		return
	}
	flusher := w.(http.Flusher)
	emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1", thinkID: "reasoning-1"}
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})

	runCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if _, err := loop.Run(runCtx, history, emitter); err != nil && runCtx.Err() == nil {
		emitter.Emit(r.Context(), agent.KindError, err.Error())
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

// writeStreamHeaders writes the SSE headers for a ui-message-stream response and
// reports whether the connection supports streaming.
func writeStreamHeaders(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("x-vercel-ai-ui-message-stream", "v1")
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return false
	}
	// Clear the per-connection write deadline for this streaming response: the
	// server's WriteTimeout would otherwise abort a long-running SSE stream (an
	// agent run can last far longer than a normal response) mid-run. Non-streaming
	// endpoints keep the timeout. Best-effort — a server without deadline support
	// is unaffected.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	return true
}

// writeSSEError streams a single error frame + finish, for failures after the
// run may have started but before/without a clean attach.
func writeSSEError(w http.ResponseWriter, msg string) {
	if !writeStreamHeaders(w) {
		return
	}
	flusher := w.(http.Flusher)
	emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1", thinkID: "reasoning-1"}
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})
	emitter.Emit(context.Background(), agent.KindError, msg)
	emitter.finish()
}

// attach streams a run's output to the client over the live StreamBroker (no
// database on the path). It subscribes first (so no live frame falls into the
// subscribe/catch-up gap), replays the retained buffer from `after`, then
// live-follows until the run settles or the client disconnects. Content frames
// come from the broker; the run's settled state comes from the runtime (the
// durable lifecycle log). Shared by serveChat (submitter, after=0) and
// serveResume (attacher): every client traverses this one path.
//
// pre is an optional set of frames written right after the `start` frame (e.g.
// the submitter's data-session frame).
func (h *Handler) attach(w http.ResponseWriter, r *http.Request, sessionID string, run session.Run, after int64, pre []chunk) {
	flusher := w.(http.Flusher)
	emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1", thinkID: "reasoning-1"}
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})
	for _, c := range pre {
		emitter.write(c)
	}

	// A settled run has no live stream: its content is durable in the message
	// store and delivered to the client via serveHistory, so there is nothing to
	// attach to here. Just close the message cleanly.
	if run.Status.Terminal() {
		emitter.finish()
		return
	}

	broker := h.runtime.Broker()

	// Subscribe to BOTH channels before any catch-up, so nothing published during
	// the catch-up below is lost: content deltas from the broker (no DB on the
	// path) and lifecycle events from the bus (running/cancelled — the latter
	// terminates this stream even when no further content frame arrives).
	contentCh, unsubContent := broker.Subscribe(sessionID, 256)
	defer unsubContent()
	lifecycleCh, unsubLifecycle := h.runtime.Subscribe(sessionID, 16)
	defer unsubLifecycle()

	// Replay the run's durable lifecycle events (running) so a client that
	// attached after they were published still learns the run started. Lifecycle
	// is low-volume, so a durable replay here is cheap and off the hot path.
	if lifecycle, err := h.runtime.Replay(r.Context(), run.ID, 0); err == nil {
		for _, e := range lifecycle {
			if agent.EventKind(e.Kind) == agent.KindUser {
				continue // the client already has its own user message
			}
			emitLifecycleEvent(r, emitter, e)
		}
	}

	// Catch up on content frames retained in the broker that this client hasn't seen.
	maxOffset := after
	if retained, err := broker.Read(r.Context(), sessionID, after); err == nil {
		for _, e := range retained {
			if e.RunID != run.ID || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
		}
	}

	// Live-follow until the run settles or the client disconnects. A periodic
	// settle check covers the case where the run finished between subscribe and
	// now (no frame will ever arrive), which the frame-driven loop can't observe.
	settlePoll := time.NewTicker(20 * time.Millisecond)
	defer settlePoll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-settlePoll.C:
			if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), sessionID); !stillActive {
				emitter.finish()
				return
			}
		case e, open := <-lifecycleCh:
			if !open {
				continue
			}
			if e.RunID != run.ID {
				continue
			}
			emitLifecycleEvent(r, emitter, e)
			// A terminal lifecycle event ends the run; the settle check will close
			// the stream on the next tick (giving any trailing content a chance).
		case e, open := <-contentCh:
			if !open {
				emitter.finish()
				return
			}
			if e.RunID != run.ID || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
			// The run may have settled without a further frame we can observe.
			if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), sessionID); !stillActive {
				emitter.finish()
				return
			}
		}
	}
}

// Emit implements agent.Emitter, streaming frames as the loop produces them.
// It honours ctx cancellation and reports write failures so the loop unwinds
// (and the run settles) when the client disconnects mid-run, rather than
// blocking forever on a dead connection.
func (e *sseEmitter) Emit(ctx context.Context, kind agent.EventKind, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeErr != nil {
		return e.writeErr
	}

	switch kind {
	case agent.KindRunning:
		// A run started: broadcast a transient lifecycle frame so every attached
		// client (not just the one that submitted) sees the session go running.
		e.writeRunStatus("running")
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
			// assistant-ui streams tool args via tool-call-delta frames (the start
			// frame carries only id+name). Emit the full input as one delta so the
			// live UI shows the arguments; otherwise it renders "{}" until reload
			// (the history path marshals ToolInput separately and is unaffected).
			if input, ok := m["input"]; ok && input != nil {
				if data, err := json.Marshal(input); err == nil {
					e.write(chunk{"type": "tool-call-delta", "toolCallId": id, "argsText": string(data)})
				}
			}
			e.write(chunk{"type": "tool-call-end", "toolCallId": id})
		}
	case agent.KindToolResult:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["tool_use_id"].(string)
			isErr, _ := m["is_error"].(bool)
			e.write(chunk{"type": "tool-result", "toolCallId": id, "result": m["content"], "isError": isErr})
		}
	case agent.KindSubagent:
		// Subagent progress: a transient data frame the client routes to the run
		// panel (via onData), never added to the message content.
		if m, ok := payload.(map[string]any); ok {
			e.write(chunk{"type": "data-subagent", "data": m, "transient": true})
		}
	case agent.KindDone:
		e.writeRunStatus("done")
	case agent.KindUsage:
		// Stash the run's token usage so finish() can report real counts. No frame
		// is written here; the AI SDK carries usage on the terminal finish frame.
		if in, out, ok := usageTokens(payload); ok {
			e.usageIn, e.usageOut = in, out
		}
	case agent.KindError:
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "error", "errorText": s})
		}
		e.writeRunStatus("failed")
	case agent.KindCancelled:
		// Close any open block, then flag the run cancelled via a transient data
		// frame so attached clients (and reconnects) can show it stopped early.
		// finish() still terminates the message normally.
		if e.thinkOpen {
			e.write(chunk{"type": "reasoning-end"})
			e.thinkOpen = false
		}
		if e.textStarted {
			e.write(chunk{"type": "text-end", "id": e.textID})
			e.textStarted = false
		}
		e.writeRunStatus("cancelled")
	}
	return e.writeErr
}

// writeRunStatus emits a transient data-run lifecycle frame. Attached clients
// use these to sync run state across tabs/devices (start, terminal status)
// without touching the message content.
func (e *sseEmitter) writeRunStatus(status string) {
	e.write(chunk{"type": "data-run", "data": map[string]any{"status": status}, "transient": true})
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
		"usage":        map[string]any{"inputTokens": e.usageIn, "outputTokens": e.usageOut},
	})
	e.writeRaw("data: [DONE]\n\n")
	e.flusher.Flush()
}

func (e *sseEmitter) write(c chunk) {
	e.writeRaw(sseFrame(c))
	e.flusher.Flush()
}

func (e *sseEmitter) writeRaw(s string) {
	if e.writeErr != nil {
		return
	}
	if _, err := e.w.Write([]byte(s)); err != nil {
		e.writeErr = err
	}
}

// usageTokens extracts input/output token counts from a KindUsage payload,
// tolerating both a provider.Usage value (the loop's direct-path emit) and a
// decoded JSON object (the broker/replay path, where the payload round-trips
// through storage as snake_case JSON). Returns ok=false when neither shape
// yields counts, so a bad payload never clobbers a prior value with zeros.
func usageTokens(payload any) (in, out int, ok bool) {
	switch v := payload.(type) {
	case provider.Usage:
		return v.InputTokens, v.OutputTokens, true
	case *provider.Usage:
		if v != nil {
			return v.InputTokens, v.OutputTokens, true
		}
	case map[string]any:
		vin, iok := intFromAny(v["input_tokens"])
		vout, ook := intFromAny(v["output_tokens"])
		return vin, vout, iok || ook
	}
	return 0, 0, false
}

// intFromAny reads an int from a JSON-decoded numeric value (float64 by default,
// or json.Number when the decoder uses UseNumber).
func intFromAny(x any) (int, bool) {
	switch n := x.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
