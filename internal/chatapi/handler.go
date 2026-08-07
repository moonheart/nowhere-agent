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
	"nowhere-agent/internal/toolruntime/builtin"
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
	// memInjectorFactory, when set, builds a per-request incremental memory
	// injector for the run's loop (surfaces new memories into the outgoing view,
	// never the durable history). Nil disables injection (tests).
	memInjectorFactory MemoryInjectorFactory
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

// Registry returns the handler's run-execution registry (nil until WithRuntime/
// WithRegistry). The server uses it to wire cross-cutting run behaviour (e.g.
// the approval Resume loop source) that must live on the shared registry.
func (h *Handler) Registry() *session.RunRegistry { return h.registry }

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
	mux.HandleFunc("POST /api/chat/sessions/{id}/state", h.serveSetSessionState)
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
	mux.Handle("POST /api/chat/sessions/{id}/state", auth(http.HandlerFunc(h.serveSetSessionState)))
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
	// cacheRead/cacheWrite carry the prompt-prefix cache hits (cache write is
	// Anthropic-only). They ride a separate data-usage frame so the client can
	// show cache detail the finish frame's input/output doesn't carry.
	usageIn    int
	usageOut   int
	cacheRead  int
	cacheWrite int
	// toolStarted tracks tool-call ids whose tool-call-start frame has been
	// written (a streaming KindToolArgs opens the block; the later KindToolUse
	// must not start it again). argsStreamed marks ids whose args arrived via
	// incremental tool-call-delta frames, so KindToolUse skips re-sending the
	// full input as one duplicate delta.
	toolStarted  map[string]bool
	argsStreamed map[string]bool
	// finishReason latches the run's terminal ui-message-stream finish reason
	// ("error" on KindError, "other" on KindCancelled, "length" when the final
	// step was a non-continued max_tokens truncation). Empty means unset; finish()
	// resolves it to "stop" or, at settle time, the run's terminal status.
	finishReason string
	// lastStepReason/lastStepContinued record the most recent finish-step so a
	// following terminal KindError can be classified as a truncation ("length")
	// rather than a generic error when the final step hit max_tokens without
	// continuing (the loop emits that step-finish before the terminal error).
	lastStepReason    string
	lastStepContinued bool
}

func (h *Handler) serveChat(w http.ResponseWriter, r *http.Request) {
	var req dataStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	history := toHistory(req)

	// A verdict on a parked run reuses this endpoint (and its SSE attach path)
	// rather than a separate response: resume the run, then stream its
	// continuation exactly as a freshly-submitted run would.
	if req.Approval != nil {
		h.serveChatResume(w, r, req.Approval, req.Tools)
		return
	}

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

	// Attach this session's sandbox-bound tools (file-tools) now that the
	// session id is known. The binder ensures the session's sandbox and
	// registers its file tools into the loop's registry.
	if h.bindTools != nil {
		h.bindTools(r.Context(), loop, sessID)
	}

	// Client-declared tools (general interrupt): tools the CLIENT executes,
	// declared in the request body. Register them so the loop suspends on a call
	// to one and hands it to the client. They never shadow a built-in tool.
	registerClientTools(loop, req.Tools)

	// Session-scoped middleware: memory injection (recalled memories into the
	// transient view) + image materialization (BlockImage path → base64), in
	// registration order. Compression is already on the loop from construction.
	h.bindSessionMiddleware(loop, r, sessID, lastUserText(req))

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

// serveChatResume handles POST /api/chat with an `approval` verdict: it applies
// the decision and starts a FRESH run to continue the conversation (run-stateless
// model, capability-gap O2), streaming the new run over the same ui-message-stream
// a normal chat turn uses. There is no suspended run to resume — the prior run
// ended when it surfaced the gated call; the verdict's tool_result is folded into
// the new run's history by the registry.
func (h *Handler) serveChatResume(w http.ResponseWriter, r *http.Request, av *approvalRequest, clientTools map[string]clientToolDecl) {
	if h.runtime == nil || h.registry == nil {
		http.Error(w, `{"error":"approval unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if av.ApprovalID == "" {
		http.Error(w, `{"error":"approvalId required"}`, http.StatusBadRequest)
		return
	}

	// Resolve the approval to find its session, then enforce ownership before
	// acting (the decision must not reach another user's interaction).
	ap, err := h.registry.ApprovalByID(r.Context(), av.ApprovalID)
	if err != nil {
		if errors.Is(err, session.ErrNoPendingApproval) {
			http.Error(w, `{"error":"approval not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	if _, ok := h.authorizeSession(w, r, ap.SessionID); !ok {
		return
	}
	sessID := ap.SessionID

	// Build the fresh run's loop and bind this session's tools BEFORE deciding:
	// an approved permission call executes through this same registry.
	loop := h.newLoop(r.Context(), h.system)
	if h.bindTools != nil {
		h.bindTools(r.Context(), loop, sessID)
	}
	// Re-register the client-declared tools so a resumed client_tool fold can
	// validate the returned output against the declared output schema.
	registerClientTools(loop, clientTools)
	// A verdict continues the conversation with no new user text: surface any
	// memories created since the last injection (empty query → recency order),
	// and materialize this session's images.
	h.bindSessionMiddleware(loop, r, sessID, "")

	// The run that parked this interaction settles RunDone right after emitting the
	// interrupt frame (run-stateless model). The client, driven by that frame, can
	// POST its verdict in the brief window before the run releases the
	// single-active-run lock — so wait for the session to go idle before starting
	// the verdict run, rather than 409-ing on a run that is already settling. Done
	// BEFORE Decide so a timeout leaves the interaction pending (retriable), not
	// decided-but-unsubmitted (which would lose the verdict). A genuine concurrent
	// run (another tab submitting a new turn) still trips the timeout → 409.
	if !h.waitForIdle(r.Context(), sessID, 5*time.Second) {
		http.Error(w, `{"error":"a run is already active in this session"}`, http.StatusConflict)
		return
	}

	ap2, complete, err := h.registry.RecordDecision(r.Context(), av.ApprovalID, av.Approved, av.Answer)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrNoPendingApproval):
			http.Error(w, `{"error":"approval already decided"}`, http.StatusConflict)
		default:
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		}
		return
	}

	// The batch (this run's gated calls) still has siblings pending: do NOT start
	// a run. The conversation waits for the rest of the queue; the model is only
	// re-invoked once every gated call has a verdict, so it never sees a partial
	// batch. Stream a trivial, immediately-finished message so the deciding
	// client's fetch completes; its card is already cleared and the next pending
	// card is now actionable. No content frames are emitted.
	if !complete {
		if !writeStreamHeaders(w) {
			return
		}
		flusher := w.(http.Flusher)
		emitter := &sseEmitter{w: w, flusher: flusher, msgID: uuid.NewString(), textID: "text-1", thinkID: "reasoning-1"}
		emitter.write(chunk{"type": "start", "messageId": emitter.msgID})
		emitter.finish()
		return
	}

	// Batch complete: fold every resolved interaction's tool_result into the
	// history and start a fresh run to continue the conversation.
	history, err := h.registry.FoldBatch(r.Context(), sessID, ap2.RunID, loop.Tools())
	if err != nil {
		writeSSEError(w, err.Error())
		return
	}
	run, err := h.registry.Submit(r.Context(), sessID, session.RunWork{Loop: loop, History: history})
	if err != nil {
		if errors.Is(err, session.ErrRunActive) {
			http.Error(w, `{"error":"a run is already active in this session"}`, http.StatusConflict)
			return
		}
		writeSSEError(w, err.Error())
		return
	}

	if !writeStreamHeaders(w) {
		return
	}
	pre := []chunk{{"type": "data-session", "data": map[string]any{"id": sessID}, "transient": true}}
	h.attach(w, r, sessID, run, 0, pre)
}

// waitForIdle blocks until the session has no active run (the single-active-run
// lock is free) or the timeout elapses, returning true once idle. It closes the
// resume-vs-settle race: the run that parked the interaction settles moments
// after publishing the interrupt frame, but a fast client can POST its verdict
// before the lock is released. Polling ActiveRun (which consults the same
// in-memory lock + durable backstop StartRun checks) lets the verdict run start
// as soon as the parking run is gone, instead of failing on a transient conflict.
func (h *Handler) waitForIdle(ctx context.Context, sessionID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, active, err := h.runtime.ActiveRun(ctx, sessionID); err == nil && !active {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
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

// registerClientTools attaches client-declared tools (from the request body) to
// the loop. Each becomes a toolruntime.ClientTool the loop suspends on — the
// client executes it, not the server. A declaration whose name collides with an
// already-registered (built-in) tool is skipped, so a client cannot shadow the
// server's tools.
func registerClientTools(loop *agent.Loop, decls map[string]clientToolDecl) {
	for name, d := range decls {
		if name == "" {
			continue
		}
		if _, exists := loop.Tools().Get(name); exists {
			continue // never shadow a built-in tool
		}
		// clientSide defaults to true: the only tools a client can declare are
		// client-executed. An explicit clientSide=false is honoured (the tool is
		// then registered but dispatch fails server-side — an unusual declaration).
		clientSide := d.ClientSide == nil || *d.ClientSide
		if !clientSide {
			continue
		}
		inputSchema := d.InputSchema
		if inputSchema == nil {
			inputSchema = d.Parameters // AI-SDK alias
		}
		if inputSchema == nil {
			inputSchema = map[string]any{"type": "object"}
		}
		loop.RegisterTool(builtin.NewClientTool(name, d.Description, inputSchema, d.OutputSchema))
	}
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
	// attach to here. Just close the message cleanly — with the run's real terminal
	// reason (a failed run must not finish "stop").
	if run.Status.Terminal() {
		h.settleFinish(r, emitter, sessionID, run.ID, run.Status)
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
				maxOffset = h.drainContent(r, emitter, contentCh, run.ID, maxOffset)
				h.settleFinish(r, emitter, sessionID, run.ID, "")
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
				maxOffset = h.drainContent(r, emitter, contentCh, run.ID, maxOffset)
				h.settleFinish(r, emitter, sessionID, run.ID, "")
				return
			}
			if e.RunID != run.ID || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
			// The run may have settled without a further frame we can observe.
			if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), sessionID); !stillActive {
				maxOffset = h.drainContent(r, emitter, contentCh, run.ID, maxOffset)
				h.settleFinish(r, emitter, sessionID, run.ID, "")
				return
			}
		}
	}
}

// settleFinish terminates an attached stream with the correct terminal finish
// reason. The latched reason (set when this emitter saw the terminal
// KindError/KindCancelled) is the common path — the run's terminal lifecycle
// event is persisted and published before the run settles, so it almost always
// arrived first. When it didn't (a late attacher, or the settle-poll firing
// before the buffered event drained), re-fetch the run's terminal status and
// map it: a failed run must not finish "stop" (which would show a cut-off answer
// as a clean completion). statusOverride, when terminal, is used directly and
// skips the re-fetch (the settled-run early-return already knows the status).
func (h *Handler) settleFinish(r *http.Request, e *sseEmitter, sessionID, runID string, statusOverride session.RunStatus) {
	if e.finishReason != "" {
		e.finish()
		return
	}
	status := statusOverride
	if !status.Terminal() && h.runtime != nil {
		if runs, err := h.runtime.RunsForSession(r.Context(), sessionID); err == nil {
			for _, run := range runs {
				if run.ID == runID {
					status = run.Status
					break
				}
			}
		}
	}
	reason := "stop"
	switch status {
	case session.RunFailed:
		reason = "error"
	case session.RunCancelled:
		reason = "other"
	}
	e.finishWithReason(reason)
}

// drainContent flushes any content frames still buffered on the subscription
// before the stream is settled. It closes the race where the run completes and
// its terminal lifecycle fires before the client has drained the broker backlog
// (the run finishing clears the retained frames via Settle, so a Read can't
// recover them): without this drain, fast runs — notably the step frames of a
// multi-iteration tool run — would be dropped between the last frame the client
// saw and the finish. Non-blocking: only frames already queued are taken.
func (h *Handler) drainContent(r *http.Request, emitter *sseEmitter, contentCh <-chan session.StreamEvent, runID string, maxOffset int64) int64 {
	for {
		select {
		case e, open := <-contentCh:
			if !open {
				return maxOffset
			}
			if e.RunID != runID || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
		default:
			return maxOffset
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
			e.write(chunk{"type": "text-delta", "id": e.textID, "textDelta": s})
		}
	case agent.KindStepStart:
		// A new think→tool step (multi-iteration run). No messageId — the decoder
		// falls back to the current message id.
		e.write(chunk{"type": "start-step"})
	case agent.KindStepFinish:
		// A step closed: record it for terminal-reason classification, then emit a
		// finish-step frame with the step's real usage and isContinued flag.
		if se, ok := stepEvent(payload); ok {
			e.lastStepReason = se.FinishReason
			e.lastStepContinued = se.IsContinued
			in, out := 0, 0
			if se.Usage != nil {
				in, out = se.Usage.InputTokens, se.Usage.OutputTokens
			}
			e.write(chunk{
				"type":         "finish-step",
				"finishReason": se.FinishReason,
				"usage":        map[string]any{"inputTokens": in, "outputTokens": out},
				"isContinued":  se.IsContinued,
			})
		}
	case agent.KindToolArgs:
		// Incremental tool-call arguments as the model streams them. Open the
		// block (id + name) the first time, then forward each fragment as a
		// tool-call-delta so the client renders a large input as it generates.
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			delta, _ := m["delta"].(string)
			e.writeToolCallStart(id, name)
			if delta != "" {
				e.write(chunk{"type": "tool-call-delta", "toolCallId": id, "argsText": delta})
				e.argsStreamed[id] = true
			}
		}
	case agent.KindToolUse:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			e.writeToolCallStart(id, name)
			// assistant-ui streams tool args via tool-call-delta frames (the start
			// frame carries only id+name). When the args already streamed as
			// incremental deltas (KindToolArgs) they're complete on the client, so
			// don't re-send the full input as one duplicate delta. When they did
			// NOT stream — the no-broker direct path, or a provider that closed the
			// stream without emitting args deltas — emit the full input here as one
			// delta so the live UI still shows the arguments (otherwise it renders
			// "{}" until reload; the history path marshals ToolInput separately and
			// is unaffected either way).
			if !e.argsStreamed[id] {
				if input, ok := m["input"]; ok && input != nil {
					if data, err := json.Marshal(input); err == nil {
						e.write(chunk{"type": "tool-call-delta", "toolCallId": id, "argsText": string(data)})
					}
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
	case agent.KindInterrupt:
		// Client-interaction prompt (general interrupt): stream the suspended call
		// to the client as a data-interaction frame. One frame for every kind —
		// approval (yes/no), ask_user (question card), client_tool (auto-execute).
		// The loop generated the interaction's ID when it detected the gate
		// (LangGraph-style), so the frame carries it — the card POSTs its verdict
		// with no refresh or lookup. Transient: it drives UI, not the message record.
		m, ok := payload.(map[string]any)
		if !ok {
			break
		}
		kind, _ := m["Kind"].(string)
		if kind == "" {
			kind = "approval"
		}
		toolCallID, _ := m["ToolCallID"].(string)
		toolName, _ := m["ToolName"].(string)
		args := m["Input"]
		if args == nil {
			args = map[string]any{}
		}
		e.write(chunk{"type": "data-interaction", "data": map[string]any{
			"interactionId": m["ID"],
			"approvalId":    m["ID"], // legacy alias for clients still reading it
			"kind":          kind,
			"toolCallId":    toolCallID,
			"toolName":      toolName,
			"args":          args,
		}, "transient": true})
	case agent.KindDone:
		e.writeRunStatus("done")
	case agent.KindUsage:
		// Stash the run's token usage so finish() can report real counts. Also
		// emit a data-usage frame carrying the full breakdown (incl. cache hits)
		// so the client can render token/cache detail; it's a durable (non-
		// transient) data frame so it lands in message metadata and survives a
		// history reload.
		if u, ok := usageTokens(payload); ok {
			e.usageIn, e.usageOut = u.InputTokens, u.OutputTokens
			e.cacheRead, e.cacheWrite = u.CacheReadTokens, u.CacheWriteTokens
			e.write(chunk{"type": "data-usage", "data": map[string]any{
				"inputTokens":      u.InputTokens,
				"outputTokens":     u.OutputTokens,
				"cacheReadTokens":  u.CacheReadTokens,
				"cacheWriteTokens": u.CacheWriteTokens,
			}})
		}
	case agent.KindError:
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "error", "errorText": s})
		}
		// Latch the terminal reason. A final step that hit max_tokens without
		// continuing is a truncation ("length"), not a generic failure — the loop
		// emits that non-continued length finish-step just before this error.
		if e.lastStepReason == "length" && !e.lastStepContinued {
			e.finishReason = "length"
		} else {
			e.finishReason = "error"
		}
		e.writeRunStatus("failed")
	case agent.KindCancelled:
		// Close any open block, then flag the run cancelled via a transient data
		// frame so attached clients (and reconnects) can show it stopped early.
		// finish() still terminates the message normally. The spec's FinishReason
		// union has no "cancelled", so the honest member is "other" (not "error" —
		// an intentional stop is not a failure).
		if e.thinkOpen {
			e.write(chunk{"type": "reasoning-end"})
			e.thinkOpen = false
		}
		if e.textStarted {
			e.write(chunk{"type": "text-end", "id": e.textID})
			e.textStarted = false
		}
		e.finishReason = "other"
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

// finish closes the text block, sends finish + [DONE] using the latched
// terminal reason (or "stop").
func (e *sseEmitter) finish() {
	e.finishWithReason("")
}

// finishWithReason closes the message with an explicit terminal reason. An empty
// reason falls back to the latched finishReason, then to "stop". The reason is
// what the assistant-ui accumulator turns into the message status: only
// "stop"/"unknown" render as complete, so a failed/truncated/cancelled run must
// NOT finish "stop" (that would show a cut-off answer as a clean completion).
func (e *sseEmitter) finishWithReason(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.thinkOpen {
		e.write(chunk{"type": "reasoning-end"})
		e.thinkOpen = false
	}
	if e.textStarted {
		e.write(chunk{"type": "text-end", "id": e.textID})
	}
	if reason == "" {
		reason = e.finishReason
	}
	if reason == "" {
		reason = "stop"
	}
	e.write(chunk{
		"type":         "finish",
		"finishReason": reason,
		"usage":        map[string]any{"inputTokens": e.usageIn, "outputTokens": e.usageOut},
	})
	e.writeRaw("data: [DONE]\n\n")
	e.flusher.Flush()
}

// writeToolCallStart emits a tool-call-start frame for a call at most once,
// guarding against the double-open a streaming KindToolArgs (which opens the
// block) followed by the block-stop KindToolUse (which closes it) would cause.
// Callers hold e.mu. The tracking maps are initialized lazily because emitters
// are built as struct literals at several call sites.
func (e *sseEmitter) writeToolCallStart(id, name string) {
	if id == "" {
		return
	}
	if e.toolStarted == nil {
		e.toolStarted = map[string]bool{}
		e.argsStreamed = map[string]bool{}
	}
	if e.toolStarted[id] {
		return
	}
	e.toolStarted[id] = true
	e.write(chunk{"type": "tool-call-start", "id": id, "toolCallId": id, "toolName": name})
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

// usageTokens extracts the full token usage (input/output + cache read/write)
// from a KindUsage payload, tolerating both a provider.Usage value (the loop's
// direct-path emit) and a decoded JSON object (the broker/replay path, where
// the payload round-trips through storage as snake_case JSON). Returns ok=false
// when no token keys are present, so a bad payload never clobbers a prior value.
func usageTokens(payload any) (u provider.Usage, ok bool) {
	switch v := payload.(type) {
	case provider.Usage:
		return v, true
	case *provider.Usage:
		if v != nil {
			return *v, true
		}
	case map[string]any:
		in, iok := intFromAny(v["input_tokens"])
		out, ook := intFromAny(v["output_tokens"])
		cr, _ := intFromAny(v["cache_read_tokens"])
		cw, _ := intFromAny(v["cache_write_tokens"])
		return provider.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw}, iok || ook
	}
	return provider.Usage{}, false
}

// stepEvent extracts a StepEvent from a KindStepFinish payload, tolerating both
// an agent.StepEvent value (the loop's direct-path emit) and a decoded JSON
// object (the broker/replay path, where the payload round-trips through storage
// with snake_case keys). Returns ok=false when the payload carries no step data.
func stepEvent(payload any) (se agent.StepEvent, ok bool) {
	switch v := payload.(type) {
	case agent.StepEvent:
		return v, true
	case *agent.StepEvent:
		if v != nil {
			return *v, true
		}
	case map[string]any:
		reason, _ := v["finish_reason"].(string)
		if reason == "" {
			reason, _ = v["finishReason"].(string)
		}
		cont, _ := v["is_continued"].(bool)
		if _, present := v["isContinued"]; present {
			cont, _ = v["isContinued"].(bool)
		}
		se.FinishReason = reason
		se.IsContinued = cont
		if um, ok := v["usage"].(map[string]any); ok {
			in, _ := intFromAny(um["input_tokens"])
			if i, ok2 := intFromAny(um["inputTokens"]); ok2 {
				in = i
			}
			out, _ := intFromAny(um["output_tokens"])
			if o, ok2 := intFromAny(um["outputTokens"]); ok2 {
				out = o
			}
			se.Usage = &provider.Usage{InputTokens: in, OutputTokens: out}
		}
		return se, reason != ""
	}
	return agent.StepEvent{}, false
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
