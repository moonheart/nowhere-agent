package chatapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
