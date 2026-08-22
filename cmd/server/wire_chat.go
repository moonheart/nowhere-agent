package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/inbound"
	"nowhere-agent/internal/mcp"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/permission"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/redact"
	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/schedule"
	"nowhere-agent/internal/scheduler"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/settings"
	"nowhere-agent/internal/skill"
	"nowhere-agent/internal/subagent"
	"nowhere-agent/internal/toolruntime"
	"nowhere-agent/internal/toolruntime/builtin"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/webhook"
)

// wire_chat.go — the former "provider branch": every capability whose wiring
// needs a resolvable LLM provider. Execution-permission gates, redaction, MCP,
// compression knobs, the dreaming worker, the loop factories, the session tool
// registry builder, the chat handler itself, run-completion webhooks with the
// delivery outbox, the scheduled-task trigger, and the inbound webhook
// dispatcher. Extracted from run() (see deps.go); the alias preamble keeps the
// body byte-identical to the pre-extraction block.
//
// dreamRunner and schedTrigger are written back to the deps: the consoles are
// wired regardless of whether a provider was configured, so they read the
// possibly-nil fields rather than sharing the old block's lexical scope.

func (d *serverDeps) wireChat(ctx context.Context) error {
	cfg, log, mux, protected := d.cfg, d.log, d.mux, d.protected
	settingsRuntime, settingsSync := d.settings, d.settingsSync
	pool, metrics := d.pool, d.metrics
	provResolver, recorder, enc := d.provResolver, d.recorder, d.enc
	sessionStore, sessionRuntime, runRegistry, messageStore := d.sessionStore, d.sessionRuntime, d.runRegistry, d.messageStore
	imageStore, uploadSvc := d.imageStore, d.uploadSvc
	sandboxMgr, sandboxPort, wsRoot := d.sandboxMgr, d.sandboxPort, d.wsRoot
	execEnabledFor := d.execEnabledFor
	memPort, skillEngine, agentDefPG := d.memPort, d.skillEngine, d.agentDefPG
	baseSystemFor := d.baseSystemFor
	ctxBuilder := d.ctxBuilder
	teamAttributor := d.teamAttributor
	budgetGate := d.budgetChecker.Check
	auditLogger := d.auditLogger
	identitySvc := d.identitySvc

	// The consolidation runner is written back to the deps because the
	// console's manual trigger needs it, and the console is wired regardless
	// of whether a provider was configured. The scheduled-task trigger
	// likewise: the run-now route fires through it.
	var dreamRunner *dreaming.Runner
	var schedTrigger *schedule.Trigger
	// Execution-permission gate (D10): authorize each tool call by the tool's
	// risk before dispatch. An "ask" decision suspends the run and presents an
	// approval card in the chat UI (only a client that consumes none of the
	// suspension experiences it as a plain deny). Defaults allow
	// read-only/sandbox-write/network and ask external-write; tighten via
	// PERMISSION_* env. The
	// policy is re-resolved from the runtime settings on EVERY check, so the
	// admin console retunes it live.
	// permissionDecision parses a runtime-settings permission value into a
	// Decision. The admin API validates on write, but the runtime store
	// could still hold something else (a legacy row, a manual DB edit); an
	// unknown value must NOT fail open — both gates below pass through any
	// value that is neither Ask nor Deny — so it is clamped to deny
	// (fail-closed) and logged.
	permissionDecision := clampPermissionDecision
	policyFor := func() permission.Policy {
		return permission.Policy{
			ReadOnly:      permissionDecision(settingsRuntime.String(settings.KeyPermissionReadOnly), "read_only"),
			SandboxWrite:  permissionDecision(settingsRuntime.String(settings.KeyPermissionSandboxWrite), "sandbox_write"),
			Network:       permissionDecision(settingsRuntime.String(settings.KeyPermissionNetwork), "network"),
			ExternalWrite: permissionDecision(settingsRuntime.String(settings.KeyPermissionExternalWrite), "external_write"),
		}
	}
	// permissionMode reads the session's permission mode from its state store.
	// An empty session id (a run with no session binding), a read error, or an
	// unknown value all fall back to auto — the safe default. The store read
	// is a PG round trip and the gate below consults it twice per gated tool
	// call (interaction gate, then execution gate), so the reads go through
	// a short-TTL cache: one query per window per session instead of two per
	// tool call, with the "allow all" toggle still effectively live.
	permModeCache := newStringTTLCache(2 * time.Second)
	permissionMode := func(ctx context.Context, sessionID string) string {
		if sessionID == "" {
			return chatapi.PermissionModeAuto
		}
		mode, err := permModeCache.getOrLoad(sessionID, func() (string, error) {
			v, ok, err := sessionRuntime.SessionStateKV(ctx, sessionID, chatapi.PermissionModeStateKey)
			if err != nil || !ok {
				return chatapi.PermissionModeAuto, err
			}
			var m string
			if err := json.Unmarshal(v, &m); err != nil {
				return chatapi.PermissionModeAuto, err
			}
			return m, nil
		})
		if err != nil {
			return chatapi.PermissionModeAuto
		}
		return mode
	}
	// sessionLiftsAsk reports whether the run's session has set permission
	// mode to allow_all, which lifts only the approval gate.
	sessionLiftsAsk := func(ctx context.Context) bool {
		return permissionMode(ctx, agent.SessionIDFromContext(ctx)) == chatapi.PermissionModeAllowAll
	}
	// permitAsk is the approval gate: an env "ask" decision surfaces as a
	// deny carrying the ApprovalReasonPrefix marker (the loop SUSPENDS and
	// prompts for human input, not errors). The session's permission mode
	// resolves at call time (see sessionLiftsAsk) so the client's "allow
	// all" toggle takes effect with no loop rebuild; allow_all lifts ONLY
	// this approval gate — the env deny gate below still blocks, and
	// ask_user/client_tool are unaffected.
	permitAsk := func(ctx context.Context, t toolruntime.Tool) (bool, string) {
		if permission.NewChecker(policyFor()).Check(t) != permission.Ask {
			return false, "" // not an ask-risk tool; defer to the next gate
		}
		if sessionLiftsAsk(ctx) {
			return false, ""
		}
		return true, agent.ApprovalReasonPrefix + fmt.Sprintf("%s (risk: %s)", t.Name(), t.Risk())
	}
	// permitEnv is the hard env-deny gate: an env "deny" decision blocks the
	// tool outright, and the reason is fed back to the model.
	permitEnv := func(ctx context.Context, t toolruntime.Tool) (bool, string) {
		if permission.NewChecker(policyFor()).Check(t) != permission.Deny {
			return false, ""
		}
		return true, fmt.Sprintf("%s (risk: %s) is not permitted by policy", t.Name(), t.Risk())
	}
	// permit is the GateFunc the PermissionMW middleware exposes to the loop,
	// registered ONCE per loop. The loop calls it at both gate points on every
	// tool call, so resolving the mode HERE (per call, from the run context's
	// session id) — not at registration time — keeps the client's "allow all"
	// toggle live and lets a subagent child inherit its parent session's mode
	// through the same context. Composed as first-deny-wins via agent.GateGroup
	// so the assembly point does not hand-nest closures: the approval gate
	// (which allow_all lifts) runs before the hard env-deny gate, so an env
	// deny still wins for a tool that is both askable and denyable.
	permit := agent.NewGateGroup().
		Use(permitAsk).
		Use(permitEnv).
		GateCheck()
	log.Info("execution-permission gate enabled",
		"read_only", cfg.Permission.ReadOnly, "sandbox_write", cfg.Permission.SandboxWrite,
		"network", cfg.Permission.Network, "external_write", cfg.Permission.ExternalWrite)

	// PII/secret redaction (enterprise-readiness): a Redactor is built per
	// loop from the runtime settings, so the admin console turns redaction
	// on/off or retunes strategy/categories live — the next run applies
	// the change. Boot validates the env values once (a bad env still
	// fails startup); a malformed RUNTIME value degrades to "no redaction"
	// with a log line — a console typo must not take the server down, but
	// it should be loud.
	if _, err := redact.New(redact.Config{
		Enabled:    cfg.Redact.Enabled,
		Strategy:   redact.Strategy(cfg.Redact.Strategy),
		Categories: cfg.Redact.Categories,
	}); err != nil {
		return fmt.Errorf("redact config: %w", err)
	}
	redactorFor := func() *redact.Redactor {
		r, err := redact.New(redact.Config{
			Enabled:    settingsRuntime.Bool(settings.KeyRedactEnabled),
			Strategy:   redact.Strategy(settingsRuntime.String(settings.KeyRedactStrategy)),
			Categories: settingsRuntime.String(settings.KeyRedactCategories),
		})
		if err != nil {
			log.Warn("redaction config invalid; redaction disabled for this run", "err", err)
			return nil
		}
		return r
	}

	// MCP integration (mcp capability): connect to the configured MCP
	// servers over Streamable HTTP and list their tools. Servers come from
	// MCP_SERVERS (JSON array — any number of enterprise MCP servers), or
	// the legacy MCP_ENABLED + MCP_SEARXNG_URL SearXNG integration. The
	// manager is shared across runs; the ToolBinder registers its tools
	// into each run's registry so subagents inherit them via the scoped
	// view. The connects run async (reconnectMCP, below): an unreachable/
	// slow server is a degraded capability, not a boot failure — a
	// transient network or TLS blip must not take the whole server down.
	// Tools stay unregistered until the handshake lands; a config mistake
	// surfaces as a clear startup warning and keeps retrying rather than
	// exiting.
	//
	// The manager is ALWAYS built — an empty boot config yields an empty
	// manager, not nil — so the runtime mcp_servers setting can enable MCP
	// from the admin console without a restart: applyMCP below would
	// otherwise short-circuit on a nil manager and a console-added server
	// would never take effect on a cold start without MCP_SERVERS.
	mcpManager, mcpErr := mcp.NewManagerFromJSON(initialMCPServers(cfg))
	if mcpErr != nil {
		return fmt.Errorf("mcp config: %w", mcpErr)
	}
	if mcpManager == nil {
		mcpManager = mcp.NewEmptyManager()
	} else {
		log.Info("mcp servers configured", "servers", mcpManager.ServerNames())
		for _, c := range mcpManager.Clients() {
			go reconnectMCP(ctx, c, log)
		}
	}
	// Runtime MCP reconfigure (admin console): a settings-watcher callback
	// keeps the manager reconciled with the mcp_servers setting on every
	// 5s tick. Unchanged servers keep their live session and tools; added
	// servers get a reconnect loop; removed servers' loops are cancelled.
	// A malformed runtime value is rejected by the PUT validation and,
	// should one ever reach here, keeps the previous set with a loud log.
	mcpCancels := map[string]context.CancelFunc{}
	applyMCP := func() {
		added, removed, err := mcpManager.Apply(settingsRuntime.String(settings.KeyMCPServers))
		if err != nil {
			log.Warn("mcp servers config invalid; keeping previous set", "err", err)
			return
		}
		for _, name := range removed {
			if cancel, ok := mcpCancels[name]; ok {
				cancel()
				delete(mcpCancels, name)
			}
			log.Info("mcp server removed", "server", name)
		}
		for _, c := range added {
			cctx, cancel := context.WithCancel(ctx)
			mcpCancels[c.Server()] = cancel
			go reconnectMCP(cctx, c, log)
			log.Info("mcp server added, connecting", "server", c.Server())
		}
	}
	settingsSync.Add(applyMCP)
	// Context compression (context-compression): the loop compresses its
	// working view as it approaches the model's context window, using a
	// no-tools summarize call (LLMCompressor). The compressor is built
	// per-loop below, over the CALLER's adapter and model, so team-scoped
	// keys and model overrides apply to summarize calls exactly as they do
	// to chat calls.
	//
	// The window comes from LLM_CONTEXT_WINDOW when set; otherwise it is
	// derived from the model's capability profile (models.dev-style table),
	// so a known model gets working compression out of the box. An unknown
	// model with no explicit window keeps compression disabled.
	// windowFor derives the context window for a RESOLVED target: the
	// runtime LLM_CONTEXT_WINDOW override when set, otherwise the model's
	// capability profile (models.dev-style table), so a known model gets
	// working compression out of the box. An unknown model with no explicit
	// window keeps compression disabled. Resolved per request because the
	// model — and thus its profile — follows the caller's provider
	// assignment; the override is re-read per request from the runtime
	// settings (admin console can tune it without a restart).
	windowFor := func(t providerreg.Target) int {
		if w := settingsRuntime.Int(settings.KeyLLMContextWindow); w > 0 {
			return w
		}
		if profile, ok := provider.LookupProfile(t.Vendor, t.Model); ok {
			return profile.ContextWindow
		}
		return 0
	}
	// replyBudgetFor reserves response space inside the context window. With
	// a small window configured, the 64k default would exceed it (the
	// provider can reject max_tokens beyond the window) and leave the
	// compression budget (window - reply) negative, so it is clamped to a
	// quarter of the window.
	replyBudgetFor := func(window int) int {
		replyBudget := 65536
		if window > 0 && window/4 < replyBudget {
			replyBudget = window / 4
		}
		return replyBudget
	}
	// idleTimeoutFor bounds streaming generations: if no SSE bytes arrive
	// within the runtime LLM_STREAM_IDLE_TIMEOUT, the stream fails fast.
	// 0 disables the stall guard. Defined here (before the dreaming block)
	// because the dreaming adapter is built below with the same knob.
	idleTimeoutFor := func() time.Duration {
		return settingsRuntime.Duration(settings.KeyLLMStreamIdleTimeout)
	}

	// Dreaming worker + the scheduler that drives it (capability-gaps K1+K2).
	// The worker consolidates sessions' episodes into long-term memory; the
	// scheduler fires it every DREAMING_INTERVAL. Idempotency rests on each
	// session's dreamed_seq watermark (migration 000009), not the scheduler's
	// in-memory last-run map, so the catch-up run at every boot only consolidates
	// messages beyond that mark.
	//
	// The WORKER is built whether or not the scheduler runs: DREAMING_ENABLED
	// governs the schedule, not the capability. Manual consolidation from the
	// console stays available with the schedule off, which is the point of
	// having it — an operator who wants consolidation to be deliberate rather
	// than periodic turns the timer off and keeps the button. Both the
	// schedule and the per-pass knobs (max tokens, caps, purge window) are
	// re-read from the runtime settings on every pass, so the admin console
	// tunes them without a restart.
	//
	// Dreaming is a platform background capability, so it consolidates over
	// the platform default provider resolved at boot — not a caller's team
	// assignment. When the registry has no servable platform provider,
	// dreaming stays unavailable (the console's manual trigger answers 503).
	if plat, err := provResolver.ResolveForTeam(ctx, ""); err == nil {
		if platAdapter := providerreg.BuildAdapter(plat, recorder, idleTimeoutFor()); platAdapter != nil {
			source := dreaming.NewStoreSource(sessionStore, messageStore)
			worker := dreaming.NewWorker(source, memPort,
				dreaming.NewProviderLLM(platAdapter, plat.Model),
				dreaming.Budget{MaxTokens: cfg.Dreaming.MaxTokens})
			worker.SetCaps(dreaming.Caps{
				Facts:     cfg.Dreaming.MaxFacts,
				Insights:  cfg.Dreaming.MaxInsights,
				Summaries: cfg.Dreaming.MaxSummaries,
			})
			worker.SetPurgeAfter(cfg.Dreaming.PurgeAfter)
			worker.SetLogger(log)

			// The runner serializes passes. Its base context is the root one, not a
			// request's: a manually triggered pass outlives the HTTP call that asked for
			// it, but must still stop when the server does.
			dreamRunner = dreaming.NewRunner(worker, ctx)
			dreamRunner.SetLogger(log)

			// Cross-instance serialization: every instance runs its own
			// scheduler, and two passes on different instances would both
			// read a session's dreamed_seq before either advances it — the
			// in-memory single-flight lock cannot see the other instance.
			// pg_try_advisory_lock on a fixed key makes the scheduled pass
			// and the console's manual trigger mutually exclusive across
			// the whole deployment; a pass that cannot take the lock skips
			// this round and the next tick picks up its work.
			dreamRunner.SetLock(dreaming.NewPGAdvisoryLock(pool))

			// syncDreamKnobs applies the runtime-settable budget/caps/purge
			// to the worker before each pass (scheduled AND manual — the
			// console's trigger runs through the same runner).
			dreamRunner.SetKnobSync(func() {
				worker.SetBudget(dreaming.Budget{MaxTokens: settingsRuntime.Int(settings.KeyDreamingMaxTokens)})
				worker.SetCaps(dreaming.Caps{
					Facts:     settingsRuntime.Int(settings.KeyDreamingMaxFacts),
					Insights:  settingsRuntime.Int(settings.KeyDreamingMaxInsights),
					Summaries: settingsRuntime.Int(settings.KeyDreamingMaxSummaries),
				})
				worker.SetPurgeAfter(time.Duration(settingsRuntime.Int(settings.KeyDreamingPurgeAfter)) * 24 * time.Hour)
			})
			// runDreaming gates the scheduled pass on the runtime
			// dreaming_enabled switch (off = the console's manual trigger
			// still works).
			runDreaming := func(ctx context.Context) error {
				if !settingsRuntime.Bool(settings.KeyDreamingEnabled) {
					return nil
				}
				return dreamRunner.RunScheduled(ctx)
			}
			sched, err := scheduler.New(log, scheduler.Job{
				Name:     "dreaming",
				Interval: cfg.Dreaming.Interval,
				Run:      runDreaming,
			})
			if err != nil {
				return fmt.Errorf("dreaming scheduler: %w", err)
			}
			go sched.Start(ctx)
			// Retune the cadence live: the scheduler re-reads the job's
			// interval each tick, and the settings watcher keeps it in
			// sync with the admin console within a few seconds.
			settingsSync.Add(func() {
				sched.SetInterval("dreaming", settingsRuntime.Duration(settings.KeyDreamingInterval))
			})
			log.Info("dreaming scheduler enabled",
				"interval", cfg.Dreaming.Interval, "max_tokens", cfg.Dreaming.MaxTokens,
				"cap_facts", cfg.Dreaming.MaxFacts, "cap_insights", cfg.Dreaming.MaxInsights,
				"cap_summaries", cfg.Dreaming.MaxSummaries, "purge_after", cfg.Dreaming.PurgeAfter)
		}
	} else {
		log.Warn("dreaming disabled: no platform provider configured", "err", err)
	}

	// applyStandardMiddleware registers the middleware every agent loop gets,
	// in canonical order — permission gate, context compression (when the
	// resolved model has a window), overflow retry. One assembly point so the
	// chat factory, the subagent factory, and the schedule trigger cannot
	// drift. callAdapter/model/window come from the per-request resolution.
	applyStandardMiddleware := func(loop *agent.Loop, callAdapter provider.Adapter, model string, window int, breaker *agent.CircuitBreaker) {
		// Tool authorization gates dispatch. The policy (permit) resolves the
		// per-session permission mode from the run context at call time, so one
		// registration covers every session and reacts to the live toggle.
		loop.Use(&agent.PermissionMW{Check: permit})
		// Redaction is rebuilt per loop from the runtime settings, so a
		// console change to redact_* applies to the NEXT run.
		if redactor := redactorFor(); redactor != nil {
			loop.Use(&agent.RedactMW{Redactor: redactor})
		}
		if window > 0 {
			loop.Use(&agent.CompressMW{Compressor: contextmgmt.NewLLMCompressor(callAdapter, model), Window: window, MaxTokens: replyBudgetFor(window), Breaker: breaker})
		}
		loop.Use(&agent.OverflowMW{})
	}

	// Subagent factory (subagent capability): builds a child loop for a
	// resolved definition. System prompt and model come from the definition
	// (model falls back to the parent's); the child's tool registry is set by
	// the spawn tool via WithTools. Closes over the provider so the subagent
	// package needs no wiring dependency.
	//
	// The compression circuit breakers live OUTSIDE the loop factories: a
	// factory rebuilds the loop and CompressMW every run, so a breaker held
	// on the middleware instance would reset each run and never trip.
	subCompressBreaker := &agent.CircuitBreaker{}
	chatCompressBreaker := &agent.CircuitBreaker{}

	// Sampling/reasoning knobs shared by every loop the platform builds:
	// LLM_TEMPERATURE (negative = provider default) and LLM_THINKING_BUDGET
	// (0 = no extended thinking). Adapters gate them per model profile.
	// Both are re-read from the runtime settings per loop build, so the
	// admin console tunes them without a restart.
	temperatureFor := func() *float64 {
		t := settingsRuntime.Float64(settings.KeyLLMTemperature)
		if t < 0 {
			return nil
		}
		return &t
	}
	thinkingBudgetFor := func() int {
		return settingsRuntime.Int(settings.KeyLLMThinkingBudget)
	}
	// maxIterationsFor caps the loop's think→tool→think iterations. 0 (or an
	// unset runtime value) falls back to the built-in default of 25, so a
	// fresh deployment behaves exactly like the former hardcode.
	maxIterationsFor := func() int {
		if n := settingsRuntime.Int(settings.KeyAgentMaxIterations); n > 0 {
			return n
		}
		return 25
	}
	// Agent definitions resolve through the layered store (persist-agent-defs):
	// durable PG-backed authored definitions overlaid on the code built-ins,
	// so user/team/system definitions take effect without a restart and a
	// store outage degrades to built-ins rather than failing spawns. The
	// built-in prompt language is re-read from the runtime settings, so
	// switching llm_system_lang applies to newly built resolvers.
	subResolverFor := func() *agentdef.Resolver {
		return agentdef.NewResolver(agentdef.NewStore(settingsRuntime.String(settings.KeySystemLang)), agentDefPG)
	}
	// resolveTarget picks the provider+model for a run: the caller's team
	// assignment (or the task's team, when teamID is set) falling back to the
	// platform default. modelOverride names an explicit model on the resolved
	// provider ("" = the provider's default). Fail-closed: an unknown model
	// or an empty registry errors, so a run never silently substitutes.
	resolveTarget := func(ctx context.Context, userID, teamID, modelOverride string) (providerreg.Target, provider.Adapter, string, error) {
		var (
			t   providerreg.Target
			err error
		)
		if teamID != "" {
			t, err = provResolver.ResolveForTeam(ctx, teamID)
		} else {
			t, err = provResolver.Resolve(ctx, userID)
		}
		if err != nil {
			return providerreg.Target{}, nil, "", err
		}
		// Raw-wire recording retargets live (admin console): each run syncs
		// the recorder root from the runtime settings before building the
		// adapter, so turning LLM_RAW_LOG_DIR on/off needs no restart.
		recorder.SetRoot(settingsRuntime.String(settings.KeyLLMRawLogDir))
		adapter := providerreg.BuildAdapter(t, recorder, idleTimeoutFor())
		if adapter == nil {
			return providerreg.Target{}, nil, "", fmt.Errorf("unsupported provider vendor %q", t.Vendor)
		}
		m, err := provResolver.ResolveModel(ctx, t, modelOverride)
		if err != nil {
			return providerreg.Target{}, nil, "", err
		}
		return t, adapter, m, nil
	}

	// buildLoop assembles the loop for a resolved run with the standard
	// middleware stack. Shared by the chat path, the scheduled-task trigger,
	// and the subagent factory, so they cannot drift. breaker is the caller's
	// compression circuit breaker (per-run middleware would reset each loop).
	buildLoop := func(ctx context.Context, userID, teamID, system, model string, breaker *agent.CircuitBreaker) (*agent.Loop, error) {
		t, adapter, m, err := resolveTarget(ctx, userID, teamID, model)
		if err != nil {
			return nil, err
		}
		loop := agent.New(adapter, toolruntime.NewRegistry(), agent.Config{
			Model:           m,
			System:          system,
			MaxTokens:       replyBudgetFor(windowFor(t)),
			MaxIterations:   maxIterationsFor(),
			CacheablePrefix: true,
			Temperature:     temperatureFor(),
			ThinkingBudget:  thinkingBudgetFor(),
		})
		// Token metrics (nowhere_llm_tokens_total): report the run's
		// aggregate usage once at run end. Only root loops carry the hook —
		// the subagent factory builds children separately, and their usage
		// folds into the root run's UsageScope, so nothing is counted
		// twice. The provider/model labels come from the resolved target.
		applyStandardMiddleware(loop, adapter, m, windowFor(t), breaker)
		loop.Use(usageObserver(t.Vendor, m, metrics))
		return loop, nil
	}

	// Subagent factory (subagent capability): builds a child loop for a
	// resolved definition. System prompt and model come from the definition
	// (model falls back to the parent caller's resolved default); the child's
	// tool registry is set by the spawn tool via WithTools. Resolution is per
	// request: the spawn runs on the parent run's worker context, which
	// carries the caller, so the child inherits the parent's provider
	// assignment. An unresolvable target surfaces as an error tool result.
	subFactory := func(ctx context.Context, def agentdef.AgentDef, _ int) (*agent.Loop, error) {
		userID := ""
		if u, ok := identity.UserFromContext(ctx); ok {
			userID = u.ID
		}
		maxIter := maxIterationsFor()
		if def.MaxTurns > 0 {
			maxIter = def.MaxTurns
		}
		t, adapter, m, err := resolveTarget(ctx, userID, "", def.Model)
		if err != nil {
			return nil, err
		}
		loop := agent.New(adapter, toolruntime.NewRegistry(), agent.Config{
			Model:           m,
			System:          def.System,
			MaxTokens:       replyBudgetFor(windowFor(t)),
			MaxIterations:   maxIter,
			CacheablePrefix: true,
			Temperature:     temperatureFor(),
			ThinkingBudget:  thinkingBudgetFor(),
		})
		// The child's permission policy resolves from the spawn context's session
		// id (set on the run by the registry), so it inherits the parent session's
		// permission mode.
		applyStandardMiddleware(loop, adapter, m, windowFor(t), subCompressBreaker)
		return loop, nil
	}

	// Loop factory + session tool binder, named so the approval Resume path
	// can rebuild a parked run's loop after a restart (capability-gap O2).
	//
	// Provider resolution happens here, per request (provider-registry): the
	// caller is already on the context (both call sites pass the request
	// context from a route behind RequireAuth), so a team that configured its
	// own provider gets its calls routed to that provider's key instead of the
	// platform default. Resolution failure fails the run closed — a request
	// with no servable provider must not silently fall back to a platform key
	// the operator did not configure.
	// newChatLoop is the chatapi LoopFactory, whose signature cannot surface
	// an error. An unresolvable request therefore gets a loop over a stub
	// adapter that errors on the first model call — the client sees a clear
	// "no provider available" error frame instead of a hang or a panic.
	// model is the client-requested model ("" = the resolved default):
	// chat-side selection is BEST-EFFORT, so an unknown/disabled name falls
	// back to the provider's default (a stale picker must not break chat),
	// unlike scheduled-task/agent-definition overrides, which stay
	// fail-closed inside buildLoop.
	newChatLoop := func(ctx context.Context, system, model string) *agent.Loop {
		userID := ""
		if u, ok := identity.UserFromContext(ctx); ok {
			userID = u.ID
		}
		loop, err := buildLoop(ctx, userID, "", system, model, chatCompressBreaker)
		if errors.Is(err, providerreg.ErrUnknownModel) {
			log.Warn("chat requested model not enabled on the resolved provider; using its default", "user", userID, "model", model)
			model = ""
			loop, err = buildLoop(ctx, userID, "", system, model, chatCompressBreaker)
		}
		if err != nil {
			log.Warn("chat loop build failed", "user", userID, "err", err)
			return agent.New(noProviderAdapter{}, toolruntime.NewRegistry(), agent.Config{Model: "", System: system, MaxTokens: 1024})
		}
		return loop
	}
	// buildToolRegistry assembles the full tool registry for a session, then
	// narrows it to whitelist when that is non-empty (scheduled-tasks D3): a
	// tool not on the whitelist is never registered into the loop, so the
	// model cannot call it. A nil whitelist keeps the full set (chat).
	//
	// http_request allowlist (enterprise integration): resolved PER SESSION
	// from the runtime settings (default: HTTP_TOOL_ALLOWLIST), so editing
	// the allowlist in the admin console applies to the next run — no
	// restart. An empty allowlist disables the tool (fail-closed: no
	// allowlist, no tool). Hostname targets are additionally SSRF-vetted
	// at call time (resolved addresses must be public or explicitly
	// CIDR-allowed; connections are pinned to the vetted addresses).
	httpToolFor := func() toolruntime.Tool {
		list := splitComma(settingsRuntime.String(settings.KeyHTTPToolAllowlist))
		if len(list) == 0 {
			return nil
		}
		return builtin.NewHTTPRequest(list, settingsRuntime.Duration(settings.KeyHTTPToolTimeout))
	}
	// query_db (enterprise integration): the agent runs read-only SQL
	// against operator-named business databases (default: QUERY_DB_DSNS).
	// The tool is rebuilt PER SESSION from the runtime settings, so adding
	// a database in the admin console applies to the next run — no
	// restart. A malformed entry is logged and SKIPPED — the remaining
	// valid entries still register, so one bad DSN must not disable every
	// database; only a list with no valid entry at all leaves the tool
	// unregistered (fail-closed).
	queryDBFor := func() toolruntime.Tool {
		list := splitComma(settingsRuntime.String(settings.KeyQueryDBDsns))
		if len(list) == 0 {
			return nil
		}
		dsns := map[string]string{}
		for _, entry := range list {
			name, dsn, ok := strings.Cut(entry, "=")
			if !ok || !validDBName(name) || !validDBDSN(dsn) {
				log.Warn("query_db DSN entry invalid; skipping", "entry", entry)
				continue
			}
			dsns[name] = dsn
		}
		if len(dsns) == 0 {
			return nil
		}
		tool := builtin.NewQueryDB(dsns, builtin.QueryDBOptions{
			Timeout: settingsRuntime.Duration(settings.KeyQueryDBTimeout),
			Logf: func(format string, args ...any) {
				log.Info("query_db: " + fmt.Sprintf(format, args...))
			},
		})
		return tool
	}
	buildToolRegistry := func(ctx context.Context, sessionID string, whitelist []string) *toolruntime.Registry {
		full := toolruntime.NewRegistry()
		full.SetMaxConcurrent(settingsRuntime.Int(settings.KeyHTTPToolMaxConcurrent))
		reg := full
		// Structured user questions (capability O-ask): the model asks 1–4
		// questions; the loop suspends the run on this tool and the user's
		// answer arrives as its result on resume. Always available (sandbox-
		// independent), RiskReadOnly so the permission gate leaves it to the
		// interaction gate.
		reg.Register(builtin.NewAskUser())
		// Plan/TODO tracking (capability-gap O1): the model maintains a visible
		// task list, persisted as the "plan" key of the session's generic state
		// store and pushed live to attached clients. The writer is the low-
		// coupling seam — the tool depends only on the SessionStateWriter func,
		// not on the session store. Always available (sandbox-independent),
		// RiskReadOnly.
		reg.Register(builtin.NewPlanWrite(func(ctx context.Context, key string, value any) error {
			data, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return sessionRuntime.SetSessionStateKV(ctx, sessionID, key, data)
		}))
		// Generative-UI smoke test (agent-driven UI): the tool pushes a fixed
		// test card. Always available (sandbox-independent), RiskReadOnly.
		reg.Register(builtin.NewTestUI())
		// Live progress-card demo: the tool streams progress frames through
		// the loop's generative-UI pusher while it runs.
		reg.Register(builtin.NewProgressUI())
		// http_request (enterprise integration): the agent calls external
		// HTTP APIs (internal ERP/CRM/knowledge services) confined to the
		// configured host allowlist. RiskNetwork, so the permission gate
		// governs it like MCP tools. Registered only when the allowlist is
		// non-empty — no allowlist, no tool (fail-closed). The allowlist
		// is re-read per session from the runtime settings.
		if httpTool := httpToolFor(); httpTool != nil {
			reg.Register(httpTool)
		}
		// query_db (enterprise integration): read-only SQL against the
		// named business databases. RiskReadOnly (the tool cannot mutate
		// anything by construction). Registered only when DSNs exist; the
		// DSN list is re-read per session from the runtime settings.
		if qdb := queryDBFor(); qdb != nil {
			reg.Register(qdb)
		}
		// load_skill, the memory tools, and view_image all scope to the
		// session owner's accessible scopes; resolve the session and scopes
		// ONCE here and share across the blocks (they used to run the same
		// GetSession + AccessibleScopes pair each). A failed resolution
		// disables ALL the blocks' tools — unified, fail-closed semantics:
		// before, the skill block kept trying with a system-only fallback
		// scope set and the memory block registered recall (and, on a
		// session lookup failure, skipped write/edit/forget) on scopes
		// that were never verified against the caller. Now a scope-
		// resolution failure registers none of the blocks' tools.
		sess, sessErr := sessionRuntime.GetSession(ctx, sessionID)
		scopes := []identity.ScopeRef{identity.SystemScope()}
		if sessErr == nil {
			if sc, err := identitySvc.AccessibleScopes(ctx, sess.UserID); err == nil {
				scopes = sc
			} else {
				sessErr = err
			}
		}
		// L0 skill index resolved ONCE: the load_skill registration gate
		// below and the run_skill_script script-detection loop (sandbox
		// branch) used to each call LoadL0 with the same (ctx, scopes).
		var skillL0 []skill.L0
		if sessErr == nil {
			skillL0, _ = skillEngine.LoadL0(ctx, scopes)
		}
		// Read-only load_skill (capability-gap K3a): the agent loads a skill's
		// instructions / resource files. Registered whenever any skill is
		// present (independent of the sandbox); scopes mirror the context
		// builder (caller user + teams + system). It executes nothing.
		if sessErr == nil && len(skillL0) > 0 {
			reg.Register(skill.NewLoadTool(skillEngine, scopes))
		}
		// recall_memory (type-split active-query side, capability K /
		// context-mgmt): the model fetches summary/insight and other memories
		// NOT auto-injected. Read-only; scopes mirror the context builder.
		// write_memory / edit_memory / forget_memory let the agent maintain
		// the caller's long-term memory online: all pinned to the session
		// owner's USER scope (never a model-chosen scope), with edit/forget
		// verifying the target memory's ownership before touching it.
		if memPort != nil && sessErr == nil {
			reg.Register(memory.NewRecallTool(memPort, scopes))
			reg.Register(memory.NewWriteMemoryTool(memPort, sess.UserID))
			reg.Register(memory.NewEditMemoryTool(memPort, sess.UserID))
			reg.Register(memory.NewForgetMemoryTool(memPort, sess.UserID))
		}
		if sandboxMgr != nil {
			// The session workspace is bind-mounted into the container at
			// /workspace — the container rootfs is read-only, so without
			// the mount every file-tool write fails EROFS and run_command
			// has no working directory. Pre-create the host dir (docker
			// needs the bind source to exist); the <root>/<sessionID>
			// layout mirrors the ImageStore and local-backend convention.
			workspaceDir := filepath.Join(wsRoot, sessionID)
			if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
				log.Warn("sandbox workspace create failed; run has no file tools", "session", sessionID, "dir", workspaceDir, "err", err)
			} else {
				h, err := sandboxMgr.Ensure(ctx, sessionID, sandbox.Options{
					// Egress policy is re-read from the runtime settings per
					// session, so the admin console retunes SANDBOX_NETWORK for
					// new sessions without a restart.
					WorkspaceDir: workspaceDir,
					Network:      sandbox.NetworkPolicy{Mode: sandbox.NetworkMode(settingsRuntime.String(settings.KeySandboxNetwork))},
				})
				if err != nil {
					log.Warn("sandbox ensure failed; run has no file tools", "session", sessionID, "err", err)
				} else {
					for _, t := range builtin.FileTools(sandboxPort, h) {
						reg.Register(t)
					}
					if execEnabledFor() {
						// run_command's per-call ceiling is re-read from the
						// runtime settings per session, so the admin console can
						// retune it without a restart (default 120s).
						reg.Register(builtin.NewRunCommand(sandboxPort, h, settingsRuntime.Duration(settings.KeyRunCommandTimeout)))
						// Skill L2 script execution (capability-gap K3b): ONE fixed
						// run_skill_script tool runs any visible skill's script by
						// name, resolved lazily against the caller's scopes. A single
						// constant tool — instead of one tool per script — keeps the
						// tools array (and thus the LLM's cacheable prompt prefix)
						// byte-stable no matter how many scripts exist or how often
						// skills are edited. Execution stays C17-safe: argv +
						// interpreter whitelist, no sh -c concatenation. Registered
						// only when some visible skill actually has scripts.
						for _, meta := range skillL0 {
							if len(meta.Scripts) > 0 {
								reg.Register(skill.NewRunSkillScript(skillEngine, scopes, sandboxPort, h))
								break
							}
						}
					}
				}
			}
		}
		// web_search + web_url_read backed by https://searchng.moonheart.dev
		// (replaces the legacy mcp_searxng_* MCP tools). Always available,
		// RiskNetwork so the permission gate governs them.
		reg.Register(builtin.NewWebSearch())
		reg.Register(builtin.NewWebURLRead())
		// MCP tools (network): registered into the same run registry so
		// children scoped from it inherit them. The manager is always
		// non-nil (an empty boot config builds an empty one); Tools()
		// returns nothing until a server connects.
		for _, t := range mcpManager.Tools() {
			reg.Register(t)
		}
		// view_image (image-input): a dedicated vision model backs a main model
		// without native image input. The tool resolves the image bytes through
		// this session's ImageStore, sends them to the vision adapter, and
		// returns the description as text. Registered only when the session
		// owner's resolved provider has a vision model AND an image store
		// exists; RiskReadOnly. Resolution follows the session owner (the run
		// worker context carries the caller), so team assignments apply.
		if imageStore != nil && sessErr == nil {
			if t, err := provResolver.Resolve(ctx, sess.UserID); err == nil {
				if vm, ok := provResolver.VisionModel(ctx, t); ok {
					if visionAdapter := providerreg.BuildAdapter(t, recorder, idleTimeoutFor()); visionAdapter != nil {
						reg.Register(builtin.NewViewImage(visionAdapter, vm, imageStore.ResolverFor(sessionID, sess.UserID)))
					}
				}
			}
		}
		// Subagent spawn tool: children draw from a scoped view of this run's
		// registry, so nested loops share the session's tools. Registered
		// last. Every knob is re-read from the runtime settings per session,
		// so the admin console retunes the capability live; the definition
		// resolver is rebuilt per session so llm_system_lang applies to
		// built-in subagent prompts too.
		if settingsRuntime.Bool(settings.KeySubagentEnabled) {
			reg.Register(subagent.NewSpawnTool(subResolverFor(), reg, subFactory,
				settingsRuntime.Int(settings.KeySubagentMaxDepth)).
				WithBudget(settingsRuntime.Int(settings.KeySubagentMaxTotal),
					settingsRuntime.Int(settings.KeySubagentMaxConcurrent)))
		}
		// Whitelist filter (scheduled-tasks D3): narrow the registry to the
		// task's granted tools. Nil whitelist = the full set (chat). Unknown
		// whitelist names are dropped silently (the tool simply isn't bound).
		if whitelist != nil {
			allow := map[string]bool{}
			for _, n := range whitelist {
				allow[n] = true
			}
			filtered := toolruntime.NewRegistry()
			filtered.SetMaxConcurrent(settingsRuntime.Int(settings.KeyHTTPToolMaxConcurrent))
			for _, t := range full.All() {
				if allow[t.Name()] {
					// D3: a whitelisted spawn_agent must scope children from the
					// FILTERED registry. The spawn tool was built over the full
					// registry; rebind it, or children would inherit every tool
					// of the session, whitelist notwithstanding.
					if st, ok := t.(*subagent.SpawnTool); ok {
						filtered.Register(st.WithParent(filtered))
						continue
					}
					filtered.Register(t)
				}
			}
			return filtered
		}
		return reg
	}
	// bindChatTools attaches session-scoped tools to a chat run's loop. It
	// delegates to buildToolRegistry (the full tool set) so the same builder
	// can serve the scheduled-task trigger with a whitelist filter applied.
	bindChatTools := func(ctx context.Context, loop *agent.Loop, sessionID string) {
		loop.WithTools(buildToolRegistry(ctx, sessionID, nil))
	}

	handler := chatapi.NewHandler(newChatLoop, baseSystemFor()).
		WithRuntime(sessionRuntime).
		WithRegistry(runRegistry).
		WithMessageStore(messageStore).
		WithContextBuilder(ctxBuilder).
		WithTeamAttributor(teamAttributor).
		WithBudgetGate(chatapi.BudgetChecker(budgetGate))
	if imageStore != nil {
		handler = handler.WithImageStore(imageStore)
		// Session image uploads share the user-level upload quota knobs,
		// enforced PER SESSION against the session's stored image files
		// (the sandbox workspace shares the session dir; only WebP image
		// files count). Per-user aggregation would scan every session dir
		// the user owns on each upload — too heavy for the hot path — so
		// the per-session cap is the minimal ceiling.
		handler = handler.WithImageQuota(func() upload.Quota {
			return upload.Quota{
				MaxFiles: settingsRuntime.Int(settings.KeyUploadMaxFilesPerUser),
				MaxBytes: int64(settingsRuntime.Int(settings.KeyUploadMaxBytesPerUser)),
			}
		})
	}
	if uploadSvc != nil {
		handler = handler.WithUploads(uploadSvc)
	}
	// Vision gate (image-input): resolves per request — the caller's provider
	// assignment and whether it has a vision model — so non-vision main models
	// get the view_image hint instead of useless image blocks, and team
	// providers are gated by their own model list. The gate self-disables when
	// the request's provider has no vision model.
	handler = handler.WithVisionGate(func(ctx context.Context) (string, bool) {
		u, ok := identity.UserFromContext(ctx)
		if !ok {
			return "", false
		}
		t, err := provResolver.Resolve(ctx, u.ID)
		if err != nil {
			return "", false
		}
		_, visionOK := provResolver.VisionModel(ctx, t)
		return t.Vendor, visionOK
	})
	// Model picker (chat-side model selection): lists the enabled models of
	// the provider the caller's chat runs resolve to — the same resolution
	// chat uses, so the picker can never offer a model chat would reject.
	// Only names are exposed; the resolved target's key stays server-side.
	// No resolvable provider = empty list (the picker hides).
	handler = handler.WithModelLister(func(ctx context.Context, userID string) (string, []string, error) {
		t, err := provResolver.Resolve(ctx, userID)
		if err != nil {
			if errors.Is(err, providerreg.ErrNoProvider) {
				return "", nil, nil
			}
			return "", nil, err
		}
		names, err := provResolver.EnabledModels(ctx, t)
		if err != nil {
			return "", nil, err
		}
		return t.Model, names, nil
	})
	// Incremental memory injection (capability K / context-mgmt): each run's
	// loop surfaces newly-created memories into the outgoing view (never the
	// durable history), keeping the system prefix byte-stable for caching.
	handler = handler.WithMemoryInjector(func(ctx context.Context, user identity.User, query string) agent.MemoryInjector {
		return chatapi.NewSessionMemoryInjector(memPort, identitySvc, sessionRuntime, user, query)
	})
	// Tool binder: attach session-scoped tools to each run. Always wired; the
	// binder registers tools conditionally (sandbox file tools, MCP tools,
	// recall_memory, view_image, spawn_agent) so a run gets exactly what its
	// session can serve.
	handler = handler.WithToolBinder(bindChatTools)

	// Outbound run-completion notifications (enterprise integration): one
	// shared notifier, registered as a run-done hook on the chat registry
	// (which also serves scheduled-task runs). Target resolution happens
	// per run so a task's own webhook_url wins over the global WEBHOOK_URL —
	// and runs with neither stay silent.
	//
	// SSRF guard: webhook URLs are user-written, so every delivery target
	// is screened against private/loopback ranges before any connection
	// (WEBHOOK_SSRF_ALLOWLIST opens legitimately internal targets). The
	// guard is ALWAYS built: an empty allowlist is the default and means
	// "strict: public targets only" (see settings.KeyWebhookSSRFAllowlist),
	// not "no guard" — a missing guard would silently disable screening
	// under the default configuration. A malformed allowlist CIDR fails
	// boot — a typo must not silently disable the guard either.
	webhookAllowlist := splitComma(cfg.Webhook.SSRFAllowlist)
	webhookGuard, err := newWebhookGuard(webhookAllowlist)
	if err != nil {
		return err
	}
	log.Info("webhook SSRF guard enabled", "allowlist_entries", webhookAllowlist)
	notifier := webhook.New(webhook.Options{
		Timeout:       cfg.Webhook.Timeout,
		Retries:       cfg.Webhook.Retries,
		SSRF:          webhookGuard,
		SigningSecret: cfg.Webhook.SigningSecret,
		Logger:        log,
	})
	if cfg.Webhook.SigningSecret != "" {
		log.Info("run-completion webhooks signed (X-Nowhere-Signature)")
	}
	// applyWebhookPolicy retunes the notifier from the runtime settings
	// (admin console): timeout/retries/signing secret change immediately,
	// and the SSRF allowlist swaps the guard (a malformed runtime list
	// keeps the previous guard and logs — unlike the boot allowlist,
	// which fails startup). An empty runtime list means "strict: public
	// targets only", so it builds a strict guard rather than disabling
	// screening — clearing the allowlist must tighten delivery, not open
	// it up.
	applyWebhookPolicy := func() {
		notifier.SetPolicy(
			settingsRuntime.Duration(settings.KeyWebhookTimeout),
			settingsRuntime.Int(settings.KeyWebhookRetries),
			settingsRuntime.String(settings.KeyWebhookSigningSecret),
		)
		g, err := newWebhookGuard(splitComma(settingsRuntime.String(settings.KeyWebhookSSRFAllowlist)))
		if err != nil {
			log.Warn("webhook SSRF allowlist invalid; keeping previous guard", "err", err)
			return
		}
		notifier.SetGuard(g)
	}
	// webhookTimeoutFor bounds one delivery attempt (and the outbox
	// context), falling back to 10s when unset — a console 0 must not
	// abort deliveries instantly.
	webhookTimeoutFor := func() time.Duration {
		if d := settingsRuntime.Duration(settings.KeyWebhookTimeout); d > 0 {
			return d
		}
		return 10 * time.Second
	}
	// Target resolution for a run's completion notification (webhook
	// package): the task's webhook_url wins over the inbound webhook's
	// notify_url, which wins over the global WEBHOOK_URL — read live from
	// the runtime settings, so the admin console retargets all
	// notifications without a restart. Runs with no URL anywhere stay
	// silent.
	targetResolver := webhook.NewTargetResolver(pool, func() string {
		return settingsRuntime.String(settings.KeyWebhookURL)
	})
	// runSummary returns the last assistant text of the session, truncated,
	// for the notification payload. Read from the durable message store so
	// the payload works even for a run whose live content already aged out.
	// LastAssistantText is the bounded tail read (newest assistant messages
	// only), so a long conversation does not load every row for a summary.
	runSummary := func(ctx context.Context, sessionID string) string {
		s, err := messageStore.LastAssistantText(ctx, sessionID, 50)
		if err != nil {
			return ""
		}
		r := []rune(s)
		if len(r) > 2000 {
			return string(r[:2000]) + "…"
		}
		return s
	}
	if cfg.Webhook.URL != "" {
		log.Info("run-completion webhooks enabled", "global_url", cfg.Webhook.URL, "timeout", cfg.Webhook.Timeout, "retries", cfg.Webhook.Retries)
	} else {
		log.Info("run-completion webhooks: no global WEBHOOK_URL; task-level webhook_url still applies")
	}
	// Run metrics (nowhere_runs_total): every terminal run — chat,
	// scheduled, inbound-triggered — is counted once at settlement, keyed
	// by outcome. The webhook hook and this hook both live on the same
	// registry; the registry fires them independently.
	handler.WithRunDoneHook(func(_ context.Context, _ string, _ session.Run, status session.RunStatus) {
		metrics.RecordRun(string(status))
	})
	// Persistent delivery outbox (enterprise integration): every run-
	// completion notification is committed to a row BEFORE the first send,
	// so a crash or a slow consumer cannot lose it. A background sweeper
	// retries pending rows with backoff; admins inspect and requeue via
	// /api/admin/webhook-deliveries.
	outbox := webhook.NewDeliveryStore(pool)
	handler.WithRunDoneHook(func(ctx context.Context, sessionID string, run session.Run, status session.RunStatus) {
		// The run context may be cancelled (a cancelled run); notifications
		// still want to fire, so delivery runs on an uncancelled view. The
		// timeout is re-read from the runtime settings so a console retune
		// applies to new notifications.
		deliverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhookTimeoutFor())
		defer cancel()
		target, err := targetResolver.Resolve(deliverCtx, sessionID)
		if err != nil || target == "" {
			return // no URL (or a lookup hiccup): nothing to notify
		}
		var userID string
		if sess, err := sessionRuntime.GetSession(deliverCtx, sessionID); err == nil {
			userID = sess.UserID
		}
		payload := webhook.RunCompletedPayload{
			Event:     "run.completed",
			RunID:     run.ID,
			SessionID: sessionID,
			UserID:    userID,
			Status:    string(status),
			TeamID:    run.TeamID,
			Model:     run.Model,
			EndedAt:   time.Now().UTC(),
			Summary:   runSummary(deliverCtx, sessionID),
		}
		// Commit to the outbox first (best-effort — a DB hiccup must not
		// break the run path); a persisted row survives this process and
		// is linked to the account (ON DELETE CASCADE), so deleting the
		// account also erases its notification history.
		d, err := outbox.Enqueue(deliverCtx, run.ID, sessionID, target, userID, payload)
		if err != nil {
			log.Warn("webhook outbox enqueue failed; falling back to fire-and-forget", "run", run.ID, "err", err)
			if derr := notifier.Deliver(deliverCtx, target, payload); derr != nil {
				log.Warn("webhook delivery failed", "run", run.ID, "session", sessionID, "target", target, "err", derr)
			}
			return
		}
		// Claim the row we just enqueued BY ID (it is the only one we own):
		// the claim holds the lease, so the background sweeper cannot race
		// us and double-send — and we never steal a backlogged older row
		// the way a global oldest-first claim would.
		if _, err := outbox.ClaimByID(deliverCtx, d.ID, time.Now().UTC()); err != nil {
			log.Warn("webhook outbox claim failed; queued for retry", "delivery", d.ID, "err", err)
			return
		}
		// First attempt now; on failure the row stays pending and the
		// sweeper retries with backoff.
		if err := notifier.Deliver(deliverCtx, target, payload); err != nil {
			metrics.RecordWebhookDelivery("failed")
			log.Warn("webhook delivery failed; queued for retry", "run", run.ID, "delivery", d.ID, "err", err)
			return
		}
		metrics.RecordWebhookDelivery("delivered")
		if err := outbox.MarkDelivered(deliverCtx, d.ID, time.Now().UTC()); err != nil {
			log.Warn("webhook outbox mark delivered failed", "delivery", d.ID, "err", err)
		}
	})
	// Outbox sweeper (webhook package): retries pending deliveries with
	// backoff (1m → 5m → 15m → 1h → 4h → 12h → 24h, then dead-letter), and
	// purges dead letters older than 30 days. Claims carry a 5-minute lease,
	// so a slow in-flight attempt is not re-claimed by another instance;
	// claims are atomic, so concurrent sweepers never double-send.
	sweeper := webhook.NewSweeper(outbox, notifier, metrics.RecordWebhookDelivery, log)
	outboxSched, err := scheduler.New(log, scheduler.Job{Name: "webhook-outbox", Interval: 30 * time.Second, Run: sweeper.Sweep})
	if err != nil {
		return fmt.Errorf("webhook sweeper scheduler: %w", err)
	}
	go outboxSched.Start(ctx)
	log.Info("webhook outbox sweeper enabled (interval 30s, backoff to dead-letter)")
	// Keep the notifier's policy in sync with the runtime settings: the
	// admin console's webhook_* keys (timeout, retries, signing secret,
	// SSRF allowlist) apply to new deliveries within a few seconds.
	settingsSync.Add(applyWebhookPolicy)
	if sandboxMgr != nil {
		// Sandbox lifecycle (D13/D15): a terminal run opens the deferred-
		// stop grace period; the hourly sandbox sweep (registered above)
		// destroys the container once it expires. Sessions resumed inside
		// the grace window get a fresh sandbox: Ensure best-effort destroys
		// the stopped container first, so the fixed-name docker container
		// cannot collide on recreation.
		handler.WithRunDoneHook(func(_ context.Context, sessionID string, _ session.Run, _ session.RunStatus) {
			sandboxMgr.MarkSessionEnded(sessionID, sandboxStopGrace)
		})
		log.Info("file tools enabled (read_file/write_file/list_dir/edit_file/grep/glob/move_file/copy_file/delete_file/make_dir)")
	}
	if execEnabledFor() {
		log.Info("run_command tool enabled", "backend", cfg.Sandbox.Backend)
	}
	// MCP tool count is logged by reconnectMCP when the async connect lands;
	// at this point it is still 0, so there is nothing to report.
	if settingsRuntime.Bool(settings.KeySubagentEnabled) {
		log.Info("subagent tool enabled (spawn_agent)", "max_depth", settingsRuntime.Int(settings.KeySubagentMaxDepth))
	}
	handler.RegisterAuthed(protected)

	// Scheduled tasks (scheduled-tasks capability): the trigger scans for
	// due tasks and fires each through the SAME run path a human chat uses —
	// it rebuilds the chat loop with a whitelist-filtered tool registry
	// (buildToolRegistry) and submits via the handler's shared RunRegistry,
	// so streaming, persistence, permission, and compression are identical.
	// The trigger is declared at function scope (above) but built here, inside
	// the provider branch; the CRUD routes are wired below, outside this
	// branch, so task management stays available with no LLM configured. Only
	// firing (scheduled sweep and run-now) needs a provider. The trigger runs
	// whenever a provider exists; whether it FIRES due tasks is the runtime
	// schedule_enabled switch (admin console), so toggling it needs no
	// restart, and the scan cadence is retuned live.
	{
		schedStore := schedule.NewPGStore(pool)
		// Loop builder: rebuild the chat loop with the task's system prompt and
		// model (modelOverride "" = the resolved default), routing through the
		// task's team assignment. Tools are NOT bound here — the target session
		// is not yet known — but via WithToolBinder once the trigger resolves
		// it. An unresolvable provider/model fails the firing (retried next
		// scan), never a silent platform fallback.
		buildSchedLoop := func(ctx context.Context, task schedule.Task, system, model string) (*agent.Loop, error) {
			return buildLoop(ctx, task.UserID, task.TeamID, system, model, chatCompressBreaker)
		}
		trigger := schedule.NewTrigger(schedStore, sessionRuntime, handler.Registry(), subResolverFor().Bound(ctx), identitySvc, buildSchedLoop, pool, cfg.Schedule.ScanInterval)
		// Tool binder: narrow the session's tool registry to the task's
		// whitelist (D3) once the trigger has resolved the session id.
		trigger.WithToolBinder(func(ctx context.Context, loop *agent.Loop, sessionID string, whitelist []string) {
			loop.WithTools(buildToolRegistry(ctx, sessionID, whitelist))
		})
		trigger.WithTeamAttributor(teamAttributor)
		trigger.WithBudgetGate(schedule.BudgetChecker(budgetGate))
		// Auto-firing gates on the runtime schedule_enabled switch (off =
		// CRUD and run-now still work).
		trigger.WithEnabledFunc(func() bool {
			return settingsRuntime.Bool(settings.KeyScheduleEnabled)
		})
		trigger.SetLogger(log)
		go trigger.Start(ctx)
		// Keep the scan cadence in sync with the admin console.
		settingsSync.Add(func() {
			trigger.SetScanInterval(settingsRuntime.Duration(settings.KeyScheduleScanInterval))
		})
		schedTrigger = trigger
		log.Info("scheduled-task trigger enabled (runtime schedule_enabled switch)", "scan_interval", cfg.Schedule.ScanInterval)
	}

	// Inbound webhooks (enterprise integration): a per-user endpoint that
	// lets external systems (ERP/OA/ITSM/IM bots) start an agent run with an
	// authenticated POST — no interactive client and no SSE connection. The
	// dispatcher reuses the chat loop builder and the shared RunRegistry, so
	// a triggered run is byte-identical to a human chat run (streaming,
	// persistence, permission, compression). Completion notifications flow
	// back through the RunDoneHook above, which resolves the webhook's
	// notify_url from the session's provenance metadata.
	inboundStore := inbound.NewStore(pool)
	if enc != nil {
		inboundStore.WithEncryption(enc)
	}
	inboundDispatcher := inbound.NewDispatcher(inboundStore, sessionRuntime, handler.Registry(),
		subResolverFor().Bound(ctx), identitySvc,
		func(ctx context.Context, userID, teamID, system, model string) (*agent.Loop, error) {
			return buildLoop(ctx, userID, teamID, system, model, chatCompressBreaker)
		},
		baseSystemFor, pool).
		WithToolBinder(bindChatTools).
		WithTeamAttributor(teamAttributor).
		WithBudgetGate(inbound.BudgetChecker(budgetGate))
	inboundDispatcher.SetLogger(log)
	inboundHandler := inbound.NewHandler(inboundStore, inboundDispatcher).
		WithAudit(auditLogger)
	// The guard is always built (an empty allowlist = strict), so inbound
	// targets are screened unconditionally too.
	inboundHandler.WithURLGuard(webhookGuard)
	inboundHandler.SetLogger(log)
	inboundHandler.RegisterPublic(mux)
	inboundHandler.RegisterAuthed(protected)
	log.Info("inbound webhook endpoint enabled (POST /api/inbound/{id}, HMAC-signed)")

	// Expired inbound nonce rows are garbage the dedupe check ignores (a
	// replayed nonce outside the signature window is a fresh event); an
	// hourly pass prunes them, off the trigger hot path (ClaimNonce only
	// upserts). The grace matches the per-claim prune it replaces.
	hourlySweep(ctx, log, "inbound nonce", func() error {
		cutoff := time.Now().UTC().Add(-inbound.SignatureWindow - time.Minute)
		removed, err := inboundStore.SweepExpiredNonces(ctx, cutoff)
		if err != nil {
			return err
		}
		if removed > 0 {
			log.Info("inbound nonce sweep removed rows", "count", removed)
		}
		return nil
	})

	log.Info("chat endpoint enabled (auth required); provider+model resolved per request from the registry")
	d.dreamRunner = dreamRunner
	d.schedTrigger = schedTrigger
	return nil
}
