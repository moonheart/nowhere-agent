package chatapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime/builtin"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/workspace"
)

// LoopFactory builds an agent loop for a chat request (provider + tools wired
// by the server). system is the composed system prompt for this request
// (base + skills + recalled memory). model is the client-requested model name
// ("" = the resolved provider's default; an invalid name is the server's job
// to fall back, never a reason to fail the run). Keeping it a factory lets
// the handler stay transport-only.
type LoopFactory func(ctx context.Context, system, model string) *agent.Loop

// TeamAttributor resolves which team's provider key bills a request from this
// user (enterprise-readiness P1-3): the team id when a team key applies, ""
// when the platform key does. The server implements it over the credential
// resolver so a run is attributed to the team actually paying for it. Nil
// leaves runs unattributed (tests/dev). An error is treated as "unattributed":
// a credential-lookup hiccup must not block chat.
type TeamAttributor func(ctx context.Context, userID string) string

// BudgetChecker reports whether a run by userID (billed to teamID, or the
// platform when "") may proceed under the monthly token budget
// (enterprise-readiness P1-1). The server implements it over the quota checker;
// the handler maps a quota.ErrBudgetExceeded to 429. Nil leaves runs ungated.
type BudgetChecker func(ctx context.Context, userID, teamID string) error

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
	// uploads, when set, serves user-level image uploads (change
	// user-image-uploads): session-independent upload + owner read, which is
	// what lets a brand-new conversation's first message carry an image.
	uploads upload.Uploader
	// imageQuota, when set, caps session image uploads (POST
	// .../sessions/{id}/images) per session: the returned quota is read on
	// every upload and enforced against the session's stored image count and
	// bytes BEFORE a blob is written (mirrors the user-level upload quota;
	// nil = unlimited).
	imageQuota func() upload.Quota
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
	// attributor, when set, resolves the team whose provider key bills each run
	// (enterprise-readiness P1-3). Nil leaves runs unattributed.
	attributor TeamAttributor
	// budgetGate, when set, enforces the monthly token budget before a run
	// starts spending (enterprise-readiness P1-1). Nil leaves runs ungated.
	budgetGate BudgetChecker
	// visionGate, when set, enables the vision gate: image blocks are rewritten
	// to a view_image hint for main models without native image input
	// (image-input capability). It resolves the inputs per request — the main
	// provider's vendor and whether a vision model is available for it — so
	// team-scoped provider resolution stays live.
	visionGate VisionGateResolver
	// models, when set, serves the model picker (GET /api/chat/models): the
	// caller's resolved default model plus enabled model names. Nil serves an
	// empty list (tests / unconfigured deployments).
	models ModelLister
}

// VisionGateResolver reports the vision-gate inputs for a request: the main
// provider's vendor ("" disables the gate) and whether a vision-capable model
// is available for the request's resolved provider (false = leave image blocks
// to the adapter's own degrade path). The server implements it over the
// provider registry so the gate follows per-team resolution.
type VisionGateResolver func(ctx context.Context) (vendor string, visionAvailable bool)

// ModelLister lists the model-picker payload for a caller: the default model
// their chat runs resolve to plus every enabled model on the resolved provider
// (team assignment → platform default, the same selection chat uses). Only
// names — never credentials. An empty list (and empty default) when no
// provider serves the caller. The server implements it over the provider
// registry resolver. Nil disables the endpoint (serves an empty list).
type ModelLister func(ctx context.Context, userID string) (defaultModel string, models []string, err error)

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
	h.registry = session.NewRunRegistry(rt)
	return h
}

// WithRegistry overrides the run-execution registry (default: one built over the
// runtime in WithRuntime).
func (h *Handler) WithRegistry(rg *session.RunRegistry) *Handler {
	h.registry = rg
	return h
}

// WithRunDoneHook registers a run-completion hook on the shared registry
// (webhook notifications and other out-of-band consumers). Call after
// WithRuntime/WithRegistry; the hook fires asynchronously on run terminal.
func (h *Handler) WithRunDoneHook(hook session.RunDoneHook) *Handler {
	if h.registry != nil {
		h.registry.WithRunDoneHook(hook)
	}
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

// WithUploads wires user-level image uploads (change user-image-uploads): the
// session-independent upload endpoint and the owner-scoped read endpoint for
// "uploads/…" image references.
func (h *Handler) WithUploads(u upload.Uploader) *Handler {
	h.uploads = u
	return h
}

// WithImageQuota wires a per-session quota for session image uploads: the
// quota is read live on every upload (so an admin-console retune applies
// without a restart) and enforced against the session's stored image files.
// Nil leaves session image uploads uncapped.
func (h *Handler) WithImageQuota(q func() upload.Quota) *Handler {
	h.imageQuota = q
	return h
}

// WithContextBuilder enables memory recall + skill L0 injection into the loop.
func (h *Handler) WithContextBuilder(cb ContextBuilder) *Handler {
	h.ctxBuilder = cb
	return h
}

// WithTeamAttributor wires run billing attribution (enterprise-readiness P1-3):
// each submitted run is stamped with the team whose provider key pays for it.
func (h *Handler) WithTeamAttributor(a TeamAttributor) *Handler {
	h.attributor = a
	return h
}

// WithBudgetGate wires monthly token-budget enforcement (enterprise-readiness
// P1-1): before a run starts spending, the gate checks the caller's (and billing
// team's) current-month usage against its budget and rejects over-budget runs
// with 429. Nil leaves runs ungated.
func (h *Handler) WithBudgetGate(g BudgetChecker) *Handler {
	h.budgetGate = g
	return h
}

// WithVisionGate enables the vision gate (image-input capability) for main
// models without native image input. resolver reports, per request, the main
// provider's vendor and whether a vision model is available for the request's
// resolved provider; the gate consults the model's capability profile per send
// and rewrites image blocks to a view_image hint when the model cannot see them
// natively. Nil disables the gate.
func (h *Handler) WithVisionGate(resolver VisionGateResolver) *Handler {
	h.visionGate = resolver
	return h
}

// WithModelLister wires the model picker (GET /api/chat/models): lister
// resolves the caller's default model plus enabled models for their chat
// provider. Nil disables the endpoint (serves an empty list).
func (h *Handler) WithModelLister(l ModelLister) *Handler {
	h.models = l
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
	mux.HandleFunc("GET /api/chat/models", h.serveModels)
	mux.HandleFunc("GET /api/chat/history", h.serveHistory)
	mux.HandleFunc("POST /api/chat/resume", h.serveResume)
	mux.HandleFunc("POST /api/chat/cancel", h.serveCancel)
	mux.HandleFunc("GET /api/chat/sessions", h.serveSessions)
	mux.HandleFunc("GET /api/chat/sessions/{id}/active", h.serveSessionActive)
	mux.HandleFunc("DELETE /api/chat/sessions/{id}", h.serveDeleteSession)
	mux.HandleFunc("POST /api/chat/sessions/{id}/state", h.serveSetSessionState)
	mux.HandleFunc("GET /api/chat/sessions/{id}/files/{path...}", h.serveFile)
	mux.HandleFunc("POST /api/chat/sessions/{id}/images", h.serveImageUpload)
	mux.HandleFunc("POST /api/chat/uploads", h.serveUserImageUpload)
	mux.HandleFunc("GET /api/chat/uploads/{id}", h.serveUserFile)
}

// RegisterAuthed mounts the protected chat routes onto the group. Auth is NOT
// wrapped per route: the group applies its middleware set (auth, and anything
// added later) once at Mount time, so this handler only declares which routes
// belong to the protected tier. Each request resolves to an authenticated user
// (sessions are user-owned).
func (h *Handler) RegisterAuthed(g *httpx.Router) {
	g.HandleFunc("POST /api/chat", h.serveChat)
	g.HandleFunc("GET /api/chat/models", h.serveModels)
	g.HandleFunc("GET /api/chat/history", h.serveHistory)
	g.HandleFunc("POST /api/chat/resume", h.serveResume)
	g.HandleFunc("POST /api/chat/cancel", h.serveCancel)
	g.HandleFunc("GET /api/chat/sessions", h.serveSessions)
	g.HandleFunc("GET /api/chat/sessions/{id}/active", h.serveSessionActive)
	g.HandleFunc("DELETE /api/chat/sessions/{id}", h.serveDeleteSession)
	g.HandleFunc("POST /api/chat/sessions/{id}/state", h.serveSetSessionState)
	g.HandleFunc("GET /api/chat/sessions/{id}/files/{path...}", h.serveFile)
	g.HandleFunc("POST /api/chat/sessions/{id}/images", h.serveImageUpload)
	g.HandleFunc("POST /api/chat/uploads", h.serveUserImageUpload)
	g.HandleFunc("GET /api/chat/uploads/{id}", h.serveUserFile)
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
	// refreshDeadline re-arms the rolling per-write deadline before each frame
	// write (see writeStreamHeaders): a stalled write is a dead client and ends
	// the stream instead of wedging the attach loop. Nil in tests / emitters
	// built without a response controller.
	refreshDeadline func()
}

// streamWriteTimeout is the rolling per-write deadline for SSE frames: long
// enough that a live frame write never hits it, short enough that a half-open
// client's blocked write ends the stream instead of hanging it (and, with a
// Redis broker, feeding the slow-consumer busy loop) forever.
const streamWriteTimeout = 30 * time.Second

// settlePollSilence is how long the attach loop tolerates frame silence before
// its settle poll falls back to the ActiveRun check (see the poll in attach).
// While a run's frames flow the run is provably active, so the DB check is
// skipped; the window is the cost of noticing a settle whose terminal event
// was lost.
const settlePollSilence = 5 * time.Second

// settleCheckInitial / settleCheckMax bound the backoff between consecutive
// ActiveRun fallback checks once the attach loop has entered the silence
// fallback. The check is a DB query for a multi-instance attach (memory
// runtime miss → PGStore.ActiveRun), so checking on every 250ms poll tick
// would hit the DB at 4 qps per attached client for the whole duration of a
// silent run (a 120s run_command emits no frames); backing off 1s → 5s caps
// that at one query per 5s per client in steady state, at the price of
// noticing a dropped-terminal-event settle up to 5s later.
const (
	settleCheckInitial = 1 * time.Second
	settleCheckMax     = 5 * time.Second
)

// heartbeatInterval is how often the attach loop writes an SSE comment frame
// (": ping\n\n") while its run is silent. Comment frames are invisible to
// EventSource and assistant-ui decoders, but they keep the connection alive
// across idle-cutoff proxies (nginx proxy_read_timeout defaults to 60s) and
// refresh the rolling write deadline, so a long silent tool call (run_command
// can run for minutes) never drops the stream while the run continues
// headless. Must stay well below both the proxy cutoff and streamWriteTimeout.
const heartbeatInterval = 20 * time.Second

// newSSEEmitter builds the production emitter over w, wiring the rolling write
// deadline refresh that writeStreamHeaders armed. Tests build sseEmitter
// literals directly (no deadline refresh, which is a no-op for their recorders).
func newSSEEmitter(w http.ResponseWriter, flusher http.Flusher, msgID, textID, thinkID string) *sseEmitter {
	e := &sseEmitter{w: w, flusher: flusher, msgID: msgID, textID: textID, thinkID: thinkID}
	e.refreshDeadline = func() {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	}
	return e
}

// serveModels answers GET /api/chat/models with the model-picker payload: the
// caller's resolved default model plus every enabled model on the provider
// their chat runs resolve to. An empty list when no provider serves the caller
// (or no lister is wired) — the picker hides rather than failing chat. Names
// only; the server's lister never exposes credentials.
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Default string   `json:"default"`
		Models  []string `json:"models"`
	}
	resp := response{}
	if h.models != nil {
		userID := ""
		if u, ok := identity.UserFromContext(r.Context()); ok {
			userID = u.ID
		}
		def, names, err := h.models(r.Context(), userID)
		if err != nil {
			slog.Warn("list chat models", "user", userID, "err", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		resp.Default, resp.Models = def, names
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) serveChat(w http.ResponseWriter, r *http.Request) {
	// Preflight streamability: every path below ends in an SSE stream, and a
	// non-flushable writer must fail BEFORE any side effect (a submitted run
	// executes headlessly; a recorded decision consumes the verdict) — not
	// after, which is how a wrapped ResponseWriter once 500'd while the run
	// went on to suspend with no client attached.
	if _, ok := w.(http.Flusher); !ok {
		httpx.Error(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// maxChatBodyBytes bounds the chat request (history, tool args, context)
	// at 4 MiB — images ride the dedicated upload endpoints, not this body.
	const maxChatBodyBytes = 4 << 20
	var req dataStreamRequest
	body, err := httpx.ReadBodyMax(r, maxChatBodyBytes)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid json")
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

	// No runtime wired (tests/dev): stream the loop directly with no persistence,
	// no registry, no run-state — the pre-registry behaviour.
	if h.runtime == nil || h.registry == nil {
		h.serveChatDirect(w, r, h.newLoop(r.Context(), h.systemPromptFor(r, req), req.Model), history)
		return
	}

	// Resolve the session and submit the run to the registry, which executes it
	// on a connection-independent worker goroutine. Then this handler simply
	// attaches to the run's event stream — the identical path serveResume uses —
	// so the submitter and every attacher are symmetric consumers (D3).
	s, err := h.resolveSession(r, req)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			// Existence and ownership stay indistinguishable (both "not found"),
			// matching authorizeSession and the delete path.
			httpx.Error(w, http.StatusNotFound, "session not found")
			return
		}
		slog.Warn("resolve session", "err", err)
		writeSSEError(w, "internal error")
		return
	}
	sessID := s.ID

	// Pending-interaction gate (capability suspend-batch-snapshot): a session
	// with undecided interactions rejects new submissions, so a suspended batch
	// can never be buried under newer turns. Durable store check — correct
	// across gateway instances. Fail-open on a store error (auxiliary check,
	// mirrors the budget gate): a genuine race still trips the single-active-run
	// lock below.
	if pending, err := h.registry.PendingApprovalsForSession(r.Context(), sessID); err == nil && len(pending) > 0 {
		httpx.Error(w, http.StatusConflict, "pending_interaction")
		return
	}

	// Billing attribution (P1-3): stamp the run with the team whose provider key
	// pays for it and the model the loop runs, so per-team/per-model cost reports
	// read the run row directly instead of reconstructing membership at read time.
	var teamID string
	if h.attributor != nil {
		teamID = h.attributor(r.Context(), s.UserID)
	}

	// Budget gate (P1-1): reject before any model spend once this caller's (or
	// the billing team's) monthly budget is met. Fail-open inside the checker, so
	// reaching here with an over-budget error means a real limit, not an
	// infrastructure hiccup — answer 429 with a Retry-After hint.
	if h.budgetGate != nil {
		if err := h.budgetGate(r.Context(), s.UserID, teamID); err != nil {
			if errors.Is(err, quota.ErrBudgetExceeded) {
				w.Header().Set("Retry-After", "3600")
				httpx.Error(w, http.StatusTooManyRequests, err.Error())
				return
			}
			slog.Warn("budget gate", "err", err)
			writeSSEError(w, "internal error")
			return
		}
	}

	// Build the loop only AFTER the reject gates above: building one resolves
	// the caller's provider (DB reads + key decryption), and a request the
	// pending gate (409) or budget gate (429) rejects must not pay for it.
	loop := h.newLoop(r.Context(), h.systemPromptFor(r, req), req.Model)

	// Attach this session's sandbox-bound tools (file-tools) now that the
	// session id is known. The binder ensures the session's sandbox and
	// registers its file tools into the loop's registry.
	if h.bindTools != nil {
		h.bindTools(r.Context(), loop, sessID)
	}

	// Reconcile a decided-but-unfolded batch (a crash between the verdict
	// commit and its fold): a new message must not bury the decided batch's
	// tool_use — fold it now, through this run's registry and gate, so the run
	// this submission starts sees a complete conversation. The fold's tool
	// execution rides the loop's middleware chain (ToolExecutor) so redaction
	// and friends apply exactly as in live dispatch. Fail-open: a reconcile
	// error logs and the submission proceeds (the batch stays retriable via
	// the verdict path).
	if runID, err := h.registry.DecidedButUnfoldedRun(r.Context(), sessID); err != nil {
		slog.Warn("reconcile decided batch", "session", sessID, "err", err)
	} else if runID != "" {
		if _, err := h.registry.FoldBatch(agent.ContextWithSessionID(r.Context(), sessID), sessID, runID, loop.Tools(), session.ToolGate(loop.Gate()), loop.ToolExecutor()); err != nil {
			slog.Warn("fold decided batch on new submission", "session", sessID, "run", runID, "err", err)
		}
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
	// conversation record) in addition to the replay event below. It carries any
	// attached image blocks (image-input capability) alongside the text, so the
	// durable record persists the image path pointers.
	var userMsg *provider.Message
	if blocks := userTurnBlocks(req); len(blocks) > 0 {
		m := provider.Message{Role: provider.RoleUser, Content: blocks}
		userMsg = &m
	}

	// Authoritative history (persist-raw-messages): when this request resumes an
	// existing session and a MessageStore is wired, rebuild the conversation from
	// the durable record — with full blocks (thinking+signature, tool_use,
	// tool_result) — instead of trusting the client-sent messages, which are
	// text-only and forgeable. The new user turn is appended so the loop sees the
	// complete conversation. For a fresh session (or no store) the client history
	// is all there is, so fall back to it. The rebuild is BOUNDED (see
	// rebuildRunHistory): a very long conversation loads only its newest tail,
	// with a truncation marker so the model knows older turns exist but are not
	// loaded — in-loop compression then keeps the view within the window.
	if h.msgStore != nil && req.ThreadID != "" && s.ID == req.ThreadID {
		if history = h.rebuildRunHistory(r.Context(), sessID); len(history) > 0 {
			if userMsg != nil {
				history = append(history, *userMsg)
			}
		}
	}

	run, err := h.registry.Submit(r.Context(), sessID, session.RunWork{Loop: loop, History: history, UserMessage: userMsg, TeamID: teamID, Model: loop.Model()})
	if err != nil {
		// Single-active-run: a second client submitting while a run is in flight
		// is rejected (multi-writer prevention), not queued. Checked before any
		// SSE headers are written so the status isn't locked to 200.
		if errors.Is(err, session.ErrRunActive) {
			httpx.Error(w, http.StatusConflict, "a run is already active in this session")
			return
		}
		slog.Warn("submit run", "session", sessID, "err", err)
		writeSSEError(w, "internal error")
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

// rebuildHistoryLimit bounds the run-history rebuild: a conversation longer
// than this loads only its newest tail for the loop's starting view. The
// limit is deliberately generous (2000 messages — far beyond what fits any
// model's context window after in-loop compression), so it only ever bites on
// pathological sessions; its purpose is bounding the per-turn DB read and
// JSON decode, not the model's view.
const rebuildHistoryLimit = 2000

// rebuildRunHistory loads the session's durable messages as the run's starting
// provider history, bounded to the newest rebuildHistoryLimit messages. When
// the conversation exceeds the bound, a truncation marker message is prepended
// so the model knows older turns exist but were not loaded — the loop's
// per-send EnsurePairing then repairs whatever the cut severs at the boundary
// (a dangling tool_use gets a synthesized error result, an orphan tool_result
// is dropped), and in-loop compression keeps the view within the context
// window. Returns nil when the store is unavailable or the session has no
// durable messages — the caller keeps the client-sent history in that case.
func (h *Handler) rebuildRunHistory(ctx context.Context, sessID string) []provider.Message {
	if h.msgStore == nil {
		return nil
	}
	stored, err := h.msgStore.MessagesTail(ctx, sessID, 0, rebuildHistoryLimit+1)
	if err != nil {
		// A read hiccup must not fail the run: fall back to the client history,
		// exactly as before the bounded rebuild.
		return nil
	}
	truncated := len(stored) > rebuildHistoryLimit
	if truncated {
		stored = stored[len(stored)-rebuildHistoryLimit:]
	}
	if len(stored) == 0 {
		return nil
	}
	history := storedMessagesToProvider(stored)
	if truncated {
		history = append([]provider.Message{
			provider.TextMessage(provider.RoleUser,
				"[Earlier conversation truncated — the beginning of this conversation was not loaded for this run.]"),
		}, history...)
	}
	return history
}

// serveChatResume handles POST /api/chat with an `approval` verdict: it applies
// the decision and starts a FRESH run to continue the conversation (run-stateless
// model, capability-gap O2), streaming the new run over the same ui-message-stream
// a normal chat turn uses. There is no suspended run to resume — the prior run
// ended when it surfaced the gated call; the verdict's tool_result is folded into
// the new run's history by the registry.
func (h *Handler) serveChatResume(w http.ResponseWriter, r *http.Request, av *approvalRequest, clientTools map[string]clientToolDecl) {
	if h.runtime == nil || h.registry == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "approval unavailable")
		return
	}
	if av.ApprovalID == "" {
		httpx.Error(w, http.StatusBadRequest, "approvalId required")
		return
	}

	// Resolve the approval to find its session, then enforce ownership before
	// acting (the decision must not reach another user's interaction).
	ap, err := h.registry.ApprovalByID(r.Context(), av.ApprovalID)
	if err != nil {
		if errors.Is(err, session.ErrNoPendingApproval) {
			httpx.Error(w, http.StatusNotFound, "approval not found")
			return
		}
		slog.Warn("resolve approval", "approvalID", av.ApprovalID, "err", err)
		httpx.ErrorFrom(w, err)
		return
	}
	if _, ok := h.authorizeSession(w, r, ap.SessionID); !ok {
		return
	}
	sessID := ap.SessionID

	// Build the fresh run's loop and bind this session's tools BEFORE deciding:
	// an approved permission call executes through this same registry. The
	// system prompt goes through the ContextBuilder (skill L0 index) exactly
	// like a fresh submission; a verdict carries no new user text, so the
	// query is empty.
	loop := h.newLoop(r.Context(), h.systemPromptForText(r, ""), "")
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
		httpx.Error(w, http.StatusConflict, "a run is already active in this session")
		return
	}

	ap2, complete, err := h.registry.RecordDecision(r.Context(), av.ApprovalID, av.Approved, av.Answer)
	if errors.Is(err, session.ErrNoPendingApproval) {
		// The row exists (ApprovalByID resolved it above) but is already
		// decided. Two cases: (a) a plain duplicate verdict — the batch folded
		// already, keep the 409; (b) the decision committed but the fold did
		// NOT (a failure/crash between RecordDecision and the fold commit), in
		// which case a retry must fall through to the idempotent fold rather
		// than deadlocking on 409 — otherwise the approved call's side effects
		// never run and its tool_use dangles forever.
		ap2 = ap
		folded, pending, serr := h.registry.BatchFoldState(r.Context(), ap.RunID)
		if serr != nil {
			slog.Warn("read batch fold state", "approvalID", av.ApprovalID, "err", serr)
			httpx.ErrorFrom(w, serr)
			return
		}
		if folded {
			httpx.Error(w, http.StatusConflict, "approval already decided")
			return
		}
		complete = pending == 0
		err = nil
	}
	if err != nil {
		slog.Warn("record decision", "approvalID", av.ApprovalID, "err", err)
		httpx.ErrorFrom(w, err)
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
		emitter := newSSEEmitter(w, flusher, uuid.NewString(), "text-1", "reasoning-1")
		emitter.write(chunk{"type": "start", "messageId": emitter.msgID})
		emitter.finish()
		return
	}

	// Batch complete: fold every resolved interaction's tool_result into the
	// history and start a fresh run to continue the conversation. The loop's
	// execution gate rides along so un-gated siblings (including hard-denied
	// calls, which never become interactions) are re-authorized at fold exactly
	// as the dispatch screen would have, and the fold's tool execution goes
	// through the loop's middleware chain (ToolExecutor) so redaction and the
	// durable step intents apply exactly as in live dispatch. The ctx carries
	// the session id: the gate resolves the session's permission mode from it,
	// and the request ctx does not have it stamped (only run ctxs are).
	//
	// The stream headers go out BEFORE the fold: folding executes the approved
	// tools synchronously and an approved run_command can run for minutes,
	// which the server's WriteTimeout (60s default) used to kill before the
	// first byte — the client watched its verdict POST die and retried (the
	// fold itself survived server-side via context.WithoutCancel, but the UX
	// was approve → disconnect → retry). Headers arm the rolling write
	// deadline, and heartbeat comment frames during the fold keep it — and
	// idle-cutoff proxies — alive until the fresh run starts streaming. Past
	// this point the response is a stream, so failures surface as error
	// FRAMES, not HTTP statuses.
	if !writeStreamHeaders(w) {
		return
	}
	history, err := h.foldWithHeartbeat(r, w, sessID, ap2.RunID, loop)
	if err != nil {
		if r.Context().Err() == nil {
			slog.Warn("fold batch", "session", sessID, "run", ap2.RunID, "err", err)
			writeSSEError(w, "internal error")
		}
		return
	}
	run, err := h.registry.Submit(r.Context(), sessID, session.RunWork{Loop: loop, History: history})
	if err != nil {
		if errors.Is(err, session.ErrRunActive) {
			// Headers are already out, so the 409 contract is unavailable;
			// surface the conflict as a stream error frame. waitForIdle above
			// already filtered the parking run, so this is a genuinely
			// concurrent submission (another tab driving the session).
			writeSSEError(w, "a run is already active in this session")
			return
		}
		slog.Warn("submit run", "session", sessID, "err", err)
		writeSSEError(w, "internal error")
		return
	}

	pre := []chunk{{"type": "data-session", "data": map[string]any{"id": sessID}, "transient": true}}
	h.attach(w, r, sessID, run, 0, pre)
}

// foldWithHeartbeat runs the suspended-batch fold on a goroutine while the
// handler emits SSE comment heartbeats, so a long approved tool call cannot
// outrun the rolling write deadline (or an idle-cutoff proxy) before the
// verdict run starts streaming. Only comment frames are written — a typed
// chunk would corrupt the message stream the attach writes after. The
// heartbeat loop ends before this returns, so the caller's attach is the
// sole writer from then on. A client disconnect mid-fold aborts the wait,
// not the fold: FoldBatch detaches from cancellation and commits on its own,
// and the client's retry re-folds idempotently.
func (h *Handler) foldWithHeartbeat(r *http.Request, w http.ResponseWriter, sessID, runID string, loop *agent.Loop) ([]provider.Message, error) {
	type foldResult struct {
		history []provider.Message
		err     error
	}
	done := make(chan foldResult, 1)
	go func() {
		history, err := h.registry.FoldBatch(agent.ContextWithSessionID(r.Context(), sessID), sessID, runID, loop.Tools(), session.ToolGate(loop.Gate()), loop.ToolExecutor())
		done <- foldResult{history, err}
	}()
	flusher := w.(http.Flusher)
	emitter := newSSEEmitter(w, flusher, uuid.NewString(), "text-1", "reasoning-1")
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-ticker.C:
			emitter.ping()
		case res := <-done:
			return res.history, res.err
		}
	}
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
	// Poll with exponential backoff (10ms up to 200ms): the common case is a
	// parking run settling in the next instant, so the first few polls stay
	// snappy; long waits no longer hammer ActiveRun (a DB backstop) at 100Hz.
	delay := 10 * time.Millisecond
	const maxDelay = 200 * time.Millisecond
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
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
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
	emitter := newSSEEmitter(w, flusher, uuid.NewString(), "text-1", "reasoning-1")
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
	return h.systemPromptForText(r, lastUserText(req))
}

// systemPromptForText is systemPromptFor for a turn without a parsed request
// body — a verdict resume carries no new user text, so the builder runs on an
// empty query, exactly as the resume's memory injection does.
func (h *Handler) systemPromptForText(r *http.Request, text string) string {
	if h.ctxBuilder == nil {
		return h.system
	}
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		return h.system
	}
	return h.ctxBuilder.SystemPrompt(r.Context(), user, text)
}

// maxSessionTitleRunes bounds the durable session title. The title is the
// first user message verbatim, and an unbounded title would bloat the sessions
// list projection and the title trigram search index (000045) with full-text
// payloads. 200 runes + an ellipsis keeps list/search semantics intact.
const maxSessionTitleRunes = 200

// sessionTitle truncates a raw title candidate to maxSessionTitleRunes runes,
// appending an ellipsis when it had to cut. Rune-based so CJK text never gets
// split mid-character.
func sessionTitle(text string) string {
	r := []rune(text)
	if len(r) <= maxSessionTitleRunes {
		return text
	}
	return string(r[:maxSessionTitleRunes]) + "…"
}

// errSessionNotFound marks a request that EXPLICITLY named a threadId which
// does not exist or is not visible to the caller. It answers 404 instead of
// silently starting a fresh session: a shared or forged link must not land in
// a blank new conversation.
var errSessionNotFound = errors.New("session not found")

// resolveSession maps the request to a session: it resumes the session named
// by threadId when it exists and belongs to the caller; a request WITHOUT a
// threadId creates a new one for the caller; a request whose threadId does
// not exist (or is someone else's) fails with errSessionNotFound.
func (h *Handler) resolveSession(r *http.Request, req dataStreamRequest) (session.Session, error) {
	userID := ""
	if u, ok := identity.UserFromContext(r.Context()); ok {
		userID = u.ID
	}
	if req.ThreadID != "" {
		s, err := h.runtime.GetSession(r.Context(), req.ThreadID)
		if err == nil && sessionVisibleTo(s, userID) {
			return s, nil
		}
		return session.Session{}, errSessionNotFound
	}
	return h.runtime.CreateSession(r.Context(), userID, sessionTitle(lastUserText(req)))
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
		httpx.Error(w, http.StatusNotFound, "session not found")
		return session.Session{}, false
	}
	callerID := ""
	if u, ok := identity.UserFromContext(r.Context()); ok {
		callerID = u.ID
	}
	if !sessionVisibleTo(s, callerID) {
		httpx.Error(w, http.StatusForbidden, "forbidden")
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
		httpx.Error(w, http.StatusInternalServerError, "streaming unsupported")
		return false
	}
	// Rolling write deadline for this streaming response: the server's
	// WriteTimeout would otherwise abort a long-running SSE stream (an agent
	// run can last far longer than a normal response) mid-run. Instead of
	// clearing the deadline entirely — which lets a half-open client block a
	// frame write forever, wedging the attach loop and (with a Redis broker)
	// feeding a slow-consumer busy loop — arm a rolling deadline that every
	// frame write refreshes: a stalled write is a dead client and must end the
	// stream, while a live stream never hits it. Best-effort — a server
	// without deadline support is unaffected. Non-streaming endpoints keep the
	// server's WriteTimeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	return true
}

// writeSSEError streams a single error frame + finish, for failures after the
// run may have started but before/without a clean attach.
func writeSSEError(w http.ResponseWriter, msg string) {
	if !writeStreamHeaders(w) {
		return
	}
	flusher := w.(http.Flusher)
	emitter := newSSEEmitter(w, flusher, uuid.NewString(), "text-1", "reasoning-1")
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
	emitter := newSSEEmitter(w, flusher, uuid.NewString(), "text-1", "reasoning-1")
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
			if !runScoped(e.RunID, run.ID) || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
		}
	}

	// Live-follow until the run settles or the client disconnects. Settle
	// detection is event-driven: the run's terminal lifecycle event
	// (done/error/cancelled) is the primary signal, and a one-shot ActiveRun
	// check right after subscribe covers a run that settled between the
	// caller's pre-check and this loop.
	settlePoll := time.NewTicker(250 * time.Millisecond)
	defer settlePoll.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	// One-shot settle check: a run that settled since the caller's pre-check
	// will never send another frame, and catching it here is free (this is the
	// case the first poll tick used to pay for).
	if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), sessionID); !stillActive {
		maxOffset = h.drainContent(r, emitter, broker, sessionID, contentCh, run.ID, maxOffset)
		h.drainLifecycle(r, emitter, lifecycleCh, run.ID)
		h.settleFinish(r, emitter, sessionID, run.ID, "")
		return
	}

	// terminal latches once this run's terminal lifecycle event is observed.
	// From then on the poll tick closes the stream without another DB check;
	// the tick is the trailing-content grace (content may still be in the
	// broker poller's pipeline — the terminal event and the last content frame
	// travel different channels).
	terminal := false
	// lastFrame marks when this attacher last saw a frame of ITS run (content
	// or lifecycle). While frames flow the run is provably active, so the poll
	// skips its ActiveRun check entirely; it only falls back to the DB after
	// settlePollSilence of silence — a run that settled without reporting a
	// terminal event (a dropped bus event, or a run force-settled without one)
	// must be noticed somehow. Once in the fallback, consecutive checks back
	// off settleCheckBackoff (1s → 5s cap): a multi-instance attach (memory
	// runtime miss → PGStore.ActiveRun per check) costs one DB query per
	// backoff interval per client while following a silent-but-active run,
	// instead of one per 250ms poll tick. The one-shot check above still
	// bounds the settle latency at attach time, so the fallback only stretches
	// the pathological dropped-terminal-event case.
	lastFrame := time.Now()
	settleCheckBackoff := settleCheckInitial
	lastSettleCheck := time.Now()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// Heartbeat: only while the run is silent do idle-cutoff proxies
			// threaten the connection; when frames flow the stream is already
			// alive, so skip the comment frame. terminal runs close on the
			// poll tick and need no heartbeat.
			if !terminal && time.Since(lastFrame) >= heartbeatInterval {
				emitter.ping()
			}
		case <-settlePoll.C:
			// While the run's frames are flowing (or were very recent) the run
			// is provably active: skip the ActiveRun check and keep following.
			// Only after settlePollSilence of silence does the poll fall back
			// to the DB — the fallback exists for settles whose terminal event
			// was lost, and skipping it while frames flow is what keeps a
			// multi-instance attach off the DB while an active run streams.
			if !terminal && time.Since(lastFrame) <= settlePollSilence {
				continue
			}
			if !terminal {
				// Throttle the fallback itself: without the backoff this
				// branch runs on every 250ms tick for as long as a silent run
				// stays active.
				if time.Since(lastSettleCheck) < settleCheckBackoff {
					continue
				}
				lastSettleCheck = time.Now()
				if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), sessionID); stillActive {
					settleCheckBackoff *= 2
					if settleCheckBackoff > settleCheckMax {
						settleCheckBackoff = settleCheckMax
					}
					continue
				}
			}
			maxOffset = h.drainContent(r, emitter, broker, sessionID, contentCh, run.ID, maxOffset)
			h.drainLifecycle(r, emitter, lifecycleCh, run.ID)
			h.settleFinish(r, emitter, sessionID, run.ID, "")
			return
		case e, open := <-lifecycleCh:
			if !open {
				continue
			}
			if e.RunID != run.ID {
				continue
			}
			emitLifecycleEvent(r, emitter, e)
			lastFrame = time.Now()
			settleCheckBackoff = settleCheckInitial // frames flowing: next silence restarts the backoff
			if terminalLifecycle(e.Kind) {
				terminal = true
			}
		case e, open := <-contentCh:
			if !open {
				maxOffset = h.drainContent(r, emitter, broker, sessionID, contentCh, run.ID, maxOffset)
				h.drainLifecycle(r, emitter, lifecycleCh, run.ID)
				h.settleFinish(r, emitter, sessionID, run.ID, "")
				return
			}
			if !runScoped(e.RunID, run.ID) || e.Offset <= maxOffset {
				continue
			}
			// A slow consumer's live frames get dropped by the broker (mem
			// broker and Redis poller alike) — recoverable via Read. Detect the
			// hole on the next delivered frame and fill it BEFORE emitting, so
			// the stream the client renders has no silent gaps. The gap frames
			// are emitted in offset order (Read is oldest-first) and bounded
			// strictly below `e`; anything at or above it is delivered live
			// next or caught by a later hole check.
			if e.Offset > maxOffset+1 {
				maxOffset = h.fillGap(r, emitter, broker, sessionID, run.ID, maxOffset, e.Offset)
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
			lastFrame = time.Now()
			settleCheckBackoff = settleCheckInitial // frames flowing: next silence restarts the backoff
		}
	}
}

// terminalLifecycle reports whether a lifecycle event ends its run. The run's
// terminal frame (done/error/cancelled) is the attach loop's primary settle
// signal; the settle poll exists only for settles it never reported.
func terminalLifecycle(kind string) bool {
	switch agent.EventKind(kind) {
	case agent.KindDone, agent.KindError, agent.KindCancelled:
		return true
	default:
		return false
	}
}

// fillGap recovers live frames dropped for this consumer between maxOffset and
// next (the offset of the live frame just received): the broker retained them
// in its ring (Read returns everything after maxOffset), so re-read and emit
// them in offset order. Frames at or above `next` are left to the live channel
// — they arrive in publish order after `e`, or a later hole check catches them
// if they too are dropped. It returns the new max offset, which the caller
// advances past `next` next. The broker Read is non-blocking (mem ring under a
// mutex; Redis XREAD with no block) and runs on the attach's own goroutine, so
// it cannot deadlock the publish path or interleave with the select loop.
func (h *Handler) fillGap(r *http.Request, emitter *sseEmitter, broker session.StreamBroker, sessionID, runID string, maxOffset, next int64) int64 {
	gap, err := broker.Read(r.Context(), sessionID, maxOffset)
	if err != nil {
		// A read failure leaves the hole unfilled (the run's content is still
		// durable in the message store; a reload repairs the view).
		return maxOffset
	}
	for _, ge := range gap {
		if !runScoped(ge.RunID, runID) || ge.Offset <= maxOffset || ge.Offset >= next {
			continue
		}
		maxOffset = ge.Offset
		emitStreamEvent(r, emitter, ge)
	}
	return maxOffset
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

// drainLifecycle flushes lifecycle events still buffered on the subscription
// before the stream is settled. The terminal KindError/KindCancelled rides the
// lifecycle bus (not the content broker), so an attacher that observes the
// settle first — via the poll or right after a content frame — must drain this
// channel too, or the run's terminal event is stranded unread: the stream then
// ends with a status-mapped finish and NO error frame. Non-blocking, like
// drainContent.
func (h *Handler) drainLifecycle(r *http.Request, emitter *sseEmitter, lifecycleCh <-chan session.Event, runID string) {
	for {
		select {
		case e, open := <-lifecycleCh:
			if !open {
				return
			}
			if e.RunID != runID {
				continue
			}
			emitLifecycleEvent(r, emitter, e)
		default:
			return
		}
	}
}

// runScoped reports whether a content frame belongs to the stream of the run
// being attached. A frame with an EMPTY RunID is session-scoped — an
// out-of-band session_state write (Runtime.SetSessionStateKV with no active
// run, e.g. the client state endpoint between runs) — and every attached
// client of the session accepts it, so the plan panel stays live across runs.
func runScoped(eRunID, runID string) bool {
	return eRunID == runID || eRunID == ""
}

// drainContent flushes any content frames still buffered on the subscription
// before the stream is settled. It closes the race where the run completes and
// its terminal lifecycle fires before the client has drained the broker backlog
// (the run finishing schedules the retained frames for cleanup via Settle, so
// Read alone can't be relied on once it runs): without this drain, fast runs —
// notably the step frames of a multi-iteration tool run — would be dropped
// between the last frame the client saw and the finish. Non-blocking: only
// frames already queued are taken. After the channel drain, a final broker Read
// recovers frames dropped for this slow consumer that are still retained in the
// ring — the run-map removal (which settle detection observes) precedes
// broker.Settle's ring clear, so there is a window where the ring still holds
// them.
func (h *Handler) drainContent(r *http.Request, emitter *sseEmitter, broker session.StreamBroker, sessionID string, contentCh <-chan session.StreamEvent, runID string, maxOffset int64) int64 {
	for {
		select {
		case e, open := <-contentCh:
			if !open {
				return maxOffset
			}
			if !runScoped(e.RunID, runID) || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
		default:
			// Channel drained. Recover frames the broker dropped for this slow
			// consumer while they are still retained: the run-map removal the
			// settle detection observed precedes broker.Settle's ring clear, so
			// Read can still see them in this window.
			if retained, err := broker.Read(r.Context(), sessionID, maxOffset); err == nil {
				for _, ge := range retained {
					if !runScoped(ge.RunID, runID) || ge.Offset <= maxOffset {
						continue
					}
					maxOffset = ge.Offset
					emitStreamEvent(r, emitter, ge)
				}
			}
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
		interaction, ok := interactionPayload(payload)
		if !ok {
			break
		}
		kind := interaction.Kind
		if kind == "" {
			kind = "approval"
		}
		args := interaction.Input
		if args == nil {
			args = map[string]any{}
		}
		e.write(chunk{"type": "data-interaction", "data": map[string]any{
			"interactionId": interaction.ID,
			"approvalId":    interaction.ID, // legacy alias for clients still reading it
			"kind":          kind,
			"toolCallId":    interaction.ToolCallID,
			"toolName":      interaction.ToolName,
			"args":          args,
		}, "transient": true})
	case agent.KindGenerativeUI:
		// Agent-driven UI a tool result declared: a durable (non-transient) data
		// frame so the client's message accumulates a data part and history
		// reloads re-render it. Shape: {type, data:{spec}}; the client matches
		// the data part by name "generative-ui".
		if m, ok := payload.(map[string]any); ok {
			if spec, ok := m["spec"]; ok {
				e.write(chunk{"type": "data-generative-ui", "data": map[string]any{"spec": spec}})
			}
		}
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

// ping writes an SSE comment frame (": ping\n\n"). Comment lines carry no
// event data, so EventSource and assistant-ui decoders ignore them entirely;
// the frame's only job is keeping the connection alive while the run is
// silent (see heartbeatInterval) and re-arming the rolling write deadline.
func (e *sseEmitter) ping() {
	e.writeRaw(": ping\n\n")
	e.flusher.Flush()
}

func (e *sseEmitter) writeRaw(s string) {
	if e.writeErr != nil {
		return
	}
	if e.refreshDeadline != nil {
		e.refreshDeadline()
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

// interactionPayload extracts an Interaction from a KindInterrupt payload,
// tolerating an agent.Interaction value (the loop's direct-path emit — the
// serveChatDirect no-runtime path hands the struct itself) and a decoded JSON
// object (the broker/replay path, where the payload round-trips through storage
// with Go's default field-name keys). The lowercase aliases guard against a
// JSON round-trip that lowercases them. Returns ok=false when the payload
// carries no interaction data.
func interactionPayload(payload any) (in agent.Interaction, ok bool) {
	switch v := payload.(type) {
	case agent.Interaction:
		return v, true
	case *agent.Interaction:
		if v != nil {
			return *v, true
		}
	case map[string]any:
		id, _ := v["ID"].(string)
		if id == "" {
			id, _ = v["id"].(string)
		}
		kind, _ := v["Kind"].(string)
		if kind == "" {
			kind, _ = v["kind"].(string)
		}
		toolCallID, _ := v["ToolCallID"].(string)
		if toolCallID == "" {
			toolCallID, _ = v["toolCallID"].(string)
		}
		toolName, _ := v["ToolName"].(string)
		if toolName == "" {
			toolName, _ = v["toolName"].(string)
		}
		args, _ := v["Input"].(map[string]any)
		if args == nil {
			args, _ = v["input"].(map[string]any)
		}
		return agent.Interaction{ID: id, Kind: kind, ToolCallID: toolCallID, ToolName: toolName, Input: args}, true
	}
	return agent.Interaction{}, false
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
