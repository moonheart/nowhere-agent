// Command server runs the nowhere-agent gateway.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nowhere-agent/internal/adminapi"
	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/agentdefapi"
	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/config"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/logging"
	"nowhere-agent/internal/mcp"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/observability"
	"nowhere-agent/internal/oidc"
	"nowhere-agent/internal/permission"
	"nowhere-agent/internal/platform/db"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/redact"
	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/schedule"
	"nowhere-agent/internal/scheduleapi"
	"nowhere-agent/internal/scheduler"
	"nowhere-agent/internal/secrets"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/skill"
	"nowhere-agent/internal/skillapi"
	"nowhere-agent/internal/subagent"
	"nowhere-agent/internal/toolruntime"
	"nowhere-agent/internal/toolruntime/builtin"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/usage"
	"nowhere-agent/internal/workspace"

	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DB.DSN, cfg.DB.MaxOpenConns, cfg.DB.MaxIdleConns, cfg.DB.ConnMaxLifetime)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("connected to database")

	// Health probe (enterprise-readiness P0-3): /healthz reports liveness as the
	// AND of registered dependency probes, so an orchestrator can tell "process
	// alive but database dead" apart from healthy. Probes are added as each
	// dependency comes up below; Postgres is the first.
	health := observability.NewHealthz(0)
	health.Add("postgres", func(context.Context) error { return pool.Ping() })

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler())

	// Metrics (enterprise-readiness P0-3): per-route request counts and latency,
	// served at /metrics for Prometheus/VictoriaMetrics to scrape. The route
	// label is the ServeMux pattern (r.Pattern), not the raw path, so
	// /api/users/{id} stays one series regardless of how many ids exist.
	metrics := observability.NewMetrics()
	mux.Handle("GET /metrics", metrics.Handler())

	identityStore := identity.NewStore(pool)
	identitySvc := identity.NewService(identityStore)
	identityHandler := identity.NewHandler(identitySvc)
	identityHandler.Register(mux)

	// Protected route tier (httpx.Router): auth — and any future per-route
	// concern (CSRF, encryption context, tenant resolution) — is applied ONCE to
	// the whole group at Mount, instead of each handler wrapping its own routes
	// in RequireAuth. Open routes (auth, oidc, healthz, metrics) stay on the
	// outer mux; a more specific pattern there beats the "/api/" subtree.
	protected := httpx.NewRouter(identityHandler.RequireAuth)

	// Audit trail (enterprise-readiness P0): one append-only logger shared by the
	// identity handler (auth events) and the admin console (administrative and
	// credential actions). Recording is best-effort — a broken sink must never
	// take a login or an admin action down — so it is wired as an option, not a
	// hard dependency, and write failures surface only in the server log.
	auditLogger := audit.NewLogger(pool, log)
	identityHandler.WithAudit(auditLogger)

	// Single-sign-on (enterprise-readiness P1-2): when OIDC_ISSUER is set, mount
	// the authorization-code flow so users sign in via the enterprise IdP (钉钉 /
	// 企业微信 / 飞书 / any standard OIDC provider) instead of a platform
	// password. SSO is only a sign-in MECHANISM — it provisions/resolves the
	// platform account (user_identities links issuer+subject) and issues the
	// platform's own bearer token, so every downstream concern (RequireAuth,
	// teams, quotas) is unchanged. A misconfigured issuer fails the boot: better
	// to refuse to start than to offer a broken SSO button.
	if cfg.OIDC.Enabled() {
		oidcProvider, err := oidc.NewProvider(ctx, oidc.Config{
			Issuer:       cfg.OIDC.Issuer,
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			RedirectURL:  cfg.OIDC.RedirectURL,
			Scopes:       strings.Split(cfg.OIDC.Scopes, " "),
		}, nil)
		if err != nil {
			return fmt.Errorf("oidc sso: %w", err)
		}
		oidcHandler := oidc.NewHandler(oidcProvider, identityStore,
			func(ctx context.Context, u identity.User) (string, error) {
				return identitySvc.IssueToken(ctx, u)
			}).WithAudit(auditLogger)
		oidcHandler.Register(mux)
		mux.Handle("GET /auth/oidc/enabled", oidc.EnabledProbe())
		log.Info("oidc sso enabled", "issuer", cfg.OIDC.Issuer, "redirect", cfg.OIDC.RedirectURL)
	}

	// Platform-admin bootstrap (admin-console): the first account to sign up on
	// an empty database is made an admin automatically, which does nothing for a
	// deployment whose accounts predate the role. BOOTSTRAP_ADMIN_EMAIL names
	// one to promote; it is idempotent, so it can stay set, and it is the
	// recovery path if no admin remains. An email nobody holds is a warning, not
	// a boot failure — a stale value must not keep the server down.
	if email := cfg.Identity.BootstrapAdminEmail; email != "" {
		switch found, err := identitySvc.PromoteByEmail(ctx, email); {
		case err != nil:
			log.Warn("bootstrap admin promotion failed", "email", email, "err", err)
		case found:
			log.Info("bootstrap admin ensured", "email", email)
		default:
			log.Warn("bootstrap admin email matches no account", "email", email)
		}
	}

	// Provider registry (change provider-registry): DB-managed LLM providers and
	// models replace the env-var model selection (LLM_*/VISION_*) and the
	// deprecated team_api_keys mechanism. Teams select a system or team-owned
	// provider; every decision is resolved per request, so registry edits and
	// reassignments take effect without a restart.
	provStore := providerreg.NewPGStore(pool)
	if enc, err := buildEncryptor(cfg); err != nil {
		return fmt.Errorf("secrets: %w", err)
	} else if enc != nil {
		provStore.WithEncryption(enc)
		log.Info("provider registry keys encrypted at rest (AES-256-GCM)")
	} else {
		log.Warn("SECRETS_MASTER_KEY unset: provider registry keys stored PLAINTEXT; set it to enable encryption at rest")
	}
	provResolver := providerreg.NewResolver(provStore)
	recorder := provider.NewRawRecorder(cfg.LLM.RawLogDir)
	if recorder.Enabled() {
		log.Info("recording raw LLM request/response", "dir", cfg.LLM.RawLogDir)
	}
	if _, err := provResolver.ResolveForTeam(ctx, ""); err != nil {
		log.Warn("no platform provider configured; chat/schedule fail until a provider is added (see the admin console)")
	}

	// Durable session runtime over Postgres: chat requests persist as runs,
	// and the run log doubles as the episodes for dreaming.
	sessionStore := session.NewPGStore(pool)
	sessionRuntime := session.NewRuntime(sessionStore)

	// Reconcile runs stranded non-terminal by a previous process (their in-memory
	// workers died with it): mark them failed at startup so they don't read as
	// active forever and hang clients that attach to them.
	if n, err := sessionRuntime.RecoverStrandedRuns(ctx); err != nil {
		log.Warn("startup run reconciliation failed", "err", err)
	} else if n > 0 {
		log.Info("reconciled stranded runs at startup", "count", n)
	}

	// Live content broker (redis-stream-live): in-memory for single instance,
	// Redis Streams for multi-instance. Selected via STREAM_BROKER; a redis
	// broker that is unreachable at boot fails fast (a multi-instance deploy
	// with a dead broker is a misconfiguration worth surfacing).
	if cfg.Stream.Broker == "redis" {
		if err := session.PingRedis(ctx, cfg.Stream.RedisAddr); err != nil {
			return fmt.Errorf("stream broker redis at %s: %w", cfg.Stream.RedisAddr, err)
		}
		// Redis Streams carry live content; Redis Pub/Sub carries lifecycle events
		// so running/done/cancelled fan out across instances too (the in-memory bus
		// only reaches clients on this instance). Durability stays in Postgres.
		broker := session.NewRedisBroker(cfg.Stream.RedisAddr, 0, 0)
		eventBus := session.NewRedisEventBus(cfg.Stream.RedisAddr)
		sessionRuntime = sessionRuntime.WithBroker(broker).WithBus(eventBus)
		health.Add("redis", func(ctx context.Context) error {
			return session.PingRedis(ctx, cfg.Stream.RedisAddr)
		})
		log.Info("live delivery: redis streams (content) + redis pub/sub (lifecycle)", "addr", cfg.Stream.RedisAddr)
	} else {
		log.Info("live delivery: in-memory (single instance)")
	}

	// Live-delivery health: surface slow-consumer drops from the fan-out layers.
	// A rising count means attached clients are falling behind live delivery and
	// healing via Read catch-up / Replay — previously silent.
	if ds, ok := sessionRuntime.Bus().(session.DropStats); ok {
		_ = metrics.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "nowhere_session_bus_dropped_total",
			Help: "Lifecycle events dropped for slow subscribers (they heal via replay).",
		}, func() float64 { return float64(ds.DroppedTotal()) }))
	}
	if ds, ok := sessionRuntime.Broker().(session.DropStats); ok {
		_ = metrics.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "nowhere_session_broker_dropped_total",
			Help: "Live content frames dropped for slow subscribers (they heal via catch-up read).",
		}, func() float64 { return float64(ds.DroppedTotal()) }))
	}

	// Full-block conversation record (persist-raw-messages): messages are
	// persisted in original form and cross-run history is rebuilt from it.
	messageStore := session.NewPGMessageStore(pool)

	// Workspace image store: image payloads referenced by messages live as
	// WebP files under a per-session dir; the messages table holds pointers.
	var imageStore *workspace.ImageStore
	if cfg.Workspace.Dir != "" {
		imageStore = workspace.NewImageStore(cfg.Workspace.Dir)
	}
	// User-level image uploads (change user-image-uploads): session-independent
	// uploads so a brand-new conversation's first message can carry an image.
	// Blob + metadata index are wired to the chat handler (upload/serve) and the
	// console (/api/me/uploads). Requires the image store; without a workspace
	// dir the routes answer 503.
	var uploadSvc *upload.Service
	if imageStore != nil {
		uploadSvc = upload.NewService(upload.NewPGStore(pool), imageStore)
	}

	// Sandbox for built-in tools (file-tools): a per-session sandbox Manager
	// over the configured backend. The tool binder (below) ensures the session's
	// sandbox and registers its file tools for each run. "off" leaves tools
	// unregistered (pre-file-tools behaviour).
	var sandboxMgr *sandbox.Manager
	var sandboxPort sandbox.Port
	switch cfg.Sandbox.Backend {
	case "local":
		root := cfg.Sandbox.WorkspaceDir
		if root == "" {
			root = cfg.Workspace.Dir
		}
		if root == "" {
			return fmt.Errorf("SANDBOX_BACKEND=local requires SANDBOX_WORKSPACE_DIR or WORKSPACE_DIR")
		}
		sandboxPort = sandbox.NewLocalPort(root).WithShell(cfg.Sandbox.Shell)
		log.Info("sandbox backend: local fs", "root", root)
	case "docker":
		dp, err := sandbox.NewDockerPort()
		if err != nil {
			return fmt.Errorf("docker sandbox: %w", err)
		}
		sandboxPort = dp
		log.Info("sandbox backend: docker")
	case "off", "":
		log.Info("sandbox backend: off (no built-in tools)")
	default:
		return fmt.Errorf("unknown SANDBOX_BACKEND %q", cfg.Sandbox.Backend)
	}
	if sandboxPort != nil {
		sandboxMgr = sandbox.NewManager(sandboxPort)
	}

	// run_command availability: the docker backend always offers it (the command
	// is contained in the Linux container); the local backend only when
	// explicitly enabled via SANDBOX_LOCAL_EXEC, since there it runs on the host.
	execEnabled := cfg.Sandbox.Backend == "docker" ||
		(cfg.Sandbox.Backend == "local" && cfg.Sandbox.LocalExec)

	// Memory (PG+vector) and skill engine feed the loop's system prompt:
	// L0 skill index + recalled memories, scoped to the caller (task 4.5).
	memPort := memory.NewPGPort(pool)
	skillStore := skill.NewPGStore(pool)
	// Durable agent definitions (persist-agent-defs): one PG store backs both
	// the spawn resolver (inside the provider branch) and the management API
	// (outside it, so the console works with no LLM configured).
	agentDefPG := agentdef.NewPGStore(pool)
	// Seed the skill store from disk (capability-gap K3): each SKILL.md under
	// SKILLS_DIR becomes a system-scope skill, lighting up the L0 index in the
	// system prompt. Empty SKILLS_DIR leaves the runtime dormant. Scripts are
	// loaded too and run in the session sandbox (C17 fixed: interpreter-per-
	// extension, no sh -c concatenation).
	if cfg.Skills.Dir != "" {
		n, err := skill.LoadDir(ctx, skillStore, cfg.Skills.Dir)
		if err != nil {
			return fmt.Errorf("load skills from %s: %w", cfg.Skills.Dir, err)
		}
		log.Info("skills seeded from disk", "dir", cfg.Skills.Dir, "count", n)
	}
	skillEngine := skill.NewEngine(skillStore)
	baseSystem := "You are nowhere-agent, a helpful AI assistant."
	ctxBuilder := chatapi.NewContextBuilder(baseSystem, identitySvc, memPort, skillEngine)

	// The consolidation runner is declared out here because the console's manual
	// trigger needs it, and the console is wired below regardless of whether a
	// provider was configured.
	var dreamRunner *dreaming.Runner

	// The scheduled-task trigger is declared out here for the same reason: the
	// run-now route (wired with the CRUD routes below, outside the provider
	// branch) fires through it, and it only exists when a provider is configured.
	var schedTrigger *schedule.Trigger

	// Billing attribution (enterprise-readiness P1-3): a run is stamped with the
	// team whose provider assignment pays for it, so per-team cost reports read
	// the run row directly. Attribution mirrors resolution: the team is billed
	// only when its own assignment actually serves the request; anything else is
	// platform-billed. A hiccup yields "" (platform-billed), never a blocked run.
	// Shared by the chat handler and the scheduled-task trigger, which attribute
	// the same way as a human run.
	teamAttributor := func(ctx context.Context, userID string) string {
		teamID, err := provStore.UserTeam(ctx, userID)
		if err != nil || teamID == "" {
			return ""
		}
		a, err := provStore.GetTeamAssignment(ctx, teamID)
		if err != nil {
			return ""
		}
		t, err := provResolver.ResolveForTeam(ctx, teamID)
		if err != nil || t.ProviderID != a.ProviderID {
			return ""
		}
		return teamID
	}

	// Budget enforcement (enterprise-readiness P1-1): the platform records token
	// usage; this is what makes a monthly limit bite. A quota.Checker compares the
	// caller's (and billing team's) current-month billable tokens against the rows
	// in usage_budgets and rejects at submit, before any model spend. Spend lookups
	// are thin adapters over the usage store (billable = input+output, the pair
	// providers price). Fail-open inside the checker: a usage/budget DB hiccup
	// never blocks a run. Shared by the chat handler and the scheduled-task trigger.
	usageStore := usage.NewStore(pool)
	budgetChecker := quota.NewChecker(quota.NewStore(pool),
		func(ctx context.Context, userID string, from, to time.Time) (int64, error) {
			t, err := usageStore.ForUser(ctx, userID, usage.Range{From: from, To: to})
			return t.Total(), err
		},
		func(ctx context.Context, teamID string, from, to time.Time) (int64, error) {
			t, err := usageStore.ForTeam(ctx, teamID, usage.Range{From: from, To: to})
			return t.Total(), err
		})
	budgetGate := budgetChecker.Check

	// Chat endpoint: build an agent loop per request from the provider registry.
	// The loop factories resolve provider+model per request, so registry edits
	// and team reassignments take effect without a restart. A request that
	// cannot be resolved fails closed — there is no boot-time default adapter.
	{
		// Execution-permission gate (D10): authorize each tool call by the tool's
		// risk before dispatch. This server is headless, so an "ask" decision
		// denies (no interactive approver). Defaults allow read-only/sandbox-write/
		// network and deny external-write; tighten via PERMISSION_* env.
		permChecker := permission.NewChecker(permission.Policy{
			ReadOnly:      permission.Decision(cfg.Permission.ReadOnly),
			SandboxWrite:  permission.Decision(cfg.Permission.SandboxWrite),
			Network:       permission.Decision(cfg.Permission.Network),
			ExternalWrite: permission.Decision(cfg.Permission.ExternalWrite),
		})
		// permissionMode reads the session's permission mode from its state store.
		// An empty session id (a run with no session binding), a read error, or an
		// unknown value all fall back to auto — the safe default.
		permissionMode := func(ctx context.Context, sessionID string) string {
			if sessionID == "" {
				return chatapi.PermissionModeAuto
			}
			v, ok, err := sessionRuntime.SessionStateKV(ctx, sessionID, chatapi.PermissionModeStateKey)
			if err != nil || !ok {
				return chatapi.PermissionModeAuto
			}
			var mode string
			if err := json.Unmarshal(v, &mode); err != nil {
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
			if permChecker.Check(t) != permission.Ask {
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
			if permChecker.Check(t) != permission.Deny {
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

		// PII/secret redaction (enterprise-readiness): one Redactor is built once
		// and shared by every loop the platform constructs (chat, subagents,
		// scheduled tasks). It is immutable after construction, so the shared
		// instance is safe across concurrent tool dispatches. Disabled by
		// default; an invalid strategy/category fails startup rather than
		// silently redacting nothing.
		redactor, err := redact.New(redact.Config{
			Enabled:    cfg.Redact.Enabled,
			Strategy:   redact.Strategy(cfg.Redact.Strategy),
			Categories: cfg.Redact.Categories,
		})
		if err != nil {
			return fmt.Errorf("redact config: %w", err)
		}
		if redactor != nil {
			log.Info("PII/secret redaction enabled", "strategy", cfg.Redact.Strategy, "categories", cfg.Redact.Categories)
		}

		// MCP integration (mcp capability): connect to the configured SearXNG MCP
		// server over Streamable HTTP and list its tools. The client is shared
		// across runs; the ToolBinder registers its tools into each run's registry
		// so subagents inherit them via the scoped view. The connect runs async
		// (reconnectMCP, below): an unreachable/slow server is a degraded
		// capability, not a boot failure — a transient network or TLS blip must
		// not take the whole server down. Tools stay unregistered until the
		// handshake lands; a config mistake surfaces as a clear startup warning
		// and keeps retrying rather than exiting.
		var mcpClient *mcp.Client
		if cfg.MCP.Enabled {
			mcpClient = mcp.NewSearxng(cfg.MCP.SearxngURL, 0)
			go reconnectMCP(ctx, mcpClient, log)
		}
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
		// LLM_CONTEXT_WINDOW override when set, otherwise the model's capability
		// profile (models.dev-style table), so a known model gets working
		// compression out of the box. An unknown model with no explicit window
		// keeps compression disabled. Resolved per request because the model —
		// and thus its profile — follows the caller's provider assignment.
		windowFor := func(t providerreg.Target) int {
			if cfg.LLM.ContextWindow > 0 {
				return cfg.LLM.ContextWindow
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
		// than periodic turns the timer off and keeps the button.
		//
		// Dreaming is a platform background capability, so it consolidates over
		// the platform default provider resolved at boot — not a caller's team
		// assignment. When the registry has no servable platform provider,
		// dreaming stays unavailable (the console's manual trigger answers 503).
		if plat, err := provResolver.ResolveForTeam(ctx, ""); err == nil {
			if platAdapter := providerreg.BuildAdapter(plat, recorder, cfg.LLM.StreamIdleTimeout); platAdapter != nil {
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

				if cfg.Dreaming.Enabled {
					sched := scheduler.New(log, scheduler.Job{
						Name:     "dreaming",
						Interval: cfg.Dreaming.Interval,
						Run:      dreamRunner.RunScheduled,
					})
					go sched.Start(ctx)
					log.Info("dreaming scheduler enabled",
						"interval", cfg.Dreaming.Interval, "max_tokens", cfg.Dreaming.MaxTokens,
						"cap_facts", cfg.Dreaming.MaxFacts, "cap_insights", cfg.Dreaming.MaxInsights,
						"cap_summaries", cfg.Dreaming.MaxSummaries, "purge_after", cfg.Dreaming.PurgeAfter)
				} else {
					log.Info("dreaming scheduler disabled; manual consolidation still available")
				}
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
			if redactor != nil {
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
		var temperature *float64
		if cfg.LLM.Temperature >= 0 {
			t := cfg.LLM.Temperature
			temperature = &t
		}

		// Agent definitions resolve through the layered store (persist-agent-defs):
		// durable PG-backed authored definitions overlaid on the code built-ins,
		// so user/team/system definitions take effect without a restart and a
		// store outage degrades to built-ins rather than failing spawns.
		subResolver := agentdef.NewResolver(agentdef.NewStore(), agentDefPG)
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
			adapter := providerreg.BuildAdapter(t, recorder, cfg.LLM.StreamIdleTimeout)
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
				MaxIterations:   25,
				CacheablePrefix: true,
				Temperature:     temperature,
				ThinkingBudget:  cfg.LLM.ThinkingBudget,
			})
			applyStandardMiddleware(loop, adapter, m, windowFor(t), breaker)
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
			maxIter := 25
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
				Temperature:     temperature,
				ThinkingBudget:  cfg.LLM.ThinkingBudget,
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
		newChatLoop := func(ctx context.Context, system string) *agent.Loop {
			userID := ""
			if u, ok := identity.UserFromContext(ctx); ok {
				userID = u.ID
			}
			loop, err := buildLoop(ctx, userID, "", system, "", chatCompressBreaker)
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
		buildToolRegistry := func(ctx context.Context, sessionID string, whitelist []string) *toolruntime.Registry {
			full := toolruntime.NewRegistry()
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
			// Read-only load_skill (capability-gap K3a): the agent loads a skill's
			// instructions / resource files. Registered whenever any skill is
			// present (independent of the sandbox); scopes mirror the context
			// builder (caller user + teams + system). It executes nothing.
			scopes := []identity.ScopeRef{identity.SystemScope()}
			if sess, err := sessionRuntime.GetSession(ctx, sessionID); err == nil {
				if sc, err := identitySvc.AccessibleScopes(ctx, sess.UserID); err == nil {
					scopes = sc
				}
			}
			if l0, err := skillEngine.LoadL0(ctx, scopes); err == nil && len(l0) > 0 {
				reg.Register(skill.NewLoadTool(skillEngine, scopes))
			}
			// recall_memory (type-split active-query side, capability K /
			// context-mgmt): the model fetches summary/insight and other memories
			// NOT auto-injected. Read-only; scopes mirror the context builder.
			if memPort != nil {
				scopes := []identity.ScopeRef{identity.SystemScope()}
				if sess, err := sessionRuntime.GetSession(ctx, sessionID); err == nil {
					if sc, err := identitySvc.AccessibleScopes(ctx, sess.UserID); err == nil {
						scopes = sc
					}
				}
				reg.Register(memory.NewRecallTool(memPort, scopes))
			}
			if sandboxMgr != nil {
				h, err := sandboxMgr.Ensure(ctx, sessionID, sandbox.Options{
					Network: sandbox.NetworkPolicy{Mode: sandbox.NetworkMode(cfg.Sandbox.Network)},
				})
				if err != nil {
					log.Warn("sandbox ensure failed; run has no file tools", "session", sessionID, "err", err)
				} else {
					for _, t := range builtin.FileTools(sandboxPort, h) {
						reg.Register(t)
					}
					if execEnabled {
						reg.Register(builtin.NewRunCommand(sandboxPort, h))
						// Skill L2 script execution (capability-gap K3b): ONE fixed
						// run_skill_script tool runs any visible skill's script by
						// name, resolved lazily against the caller's scopes. A single
						// constant tool — instead of one tool per script — keeps the
						// tools array (and thus the LLM's cacheable prompt prefix)
						// byte-stable no matter how many scripts exist or how often
						// skills are edited. Execution stays C17-safe: argv +
						// interpreter whitelist, no sh -c concatenation. Registered
						// only when some visible skill actually has scripts.
						if l0, err := skillEngine.LoadL0(ctx, scopes); err == nil {
							for _, meta := range l0 {
								if len(meta.Scripts) > 0 {
									reg.Register(skill.NewRunSkillScript(skillEngine, scopes, sandboxPort, h))
									break
								}
							}
						}
					}
				}
			}
			// MCP tools (network): registered into the same run registry so
			// children scoped from it inherit them.
			if mcpClient != nil {
				for _, t := range mcpClient.Tools() {
					reg.Register(t)
				}
			}
			// view_image (image-input): a dedicated vision model backs a main model
			// without native image input. The tool resolves the image bytes through
			// this session's ImageStore, sends them to the vision adapter, and
			// returns the description as text. Registered only when the session
			// owner's resolved provider has a vision model AND an image store
			// exists; RiskReadOnly. Resolution follows the session owner (the run
			// worker context carries the caller), so team assignments apply.
			if imageStore != nil {
				if sess, err := sessionRuntime.GetSession(ctx, sessionID); err == nil {
					if t, err := provResolver.Resolve(ctx, sess.UserID); err == nil {
						if vm, ok := provResolver.VisionModel(ctx, t); ok {
							if visionAdapter := providerreg.BuildAdapter(t, recorder, cfg.LLM.StreamIdleTimeout); visionAdapter != nil {
								reg.Register(builtin.NewViewImage(visionAdapter, vm, imageStore.ResolverFor(sessionID, sess.UserID)))
							}
						}
					}
				}
			}
			// Subagent spawn tool: children draw from a scoped view of this run's
			// registry, so nested loops share the session's tools. Registered last.
			if cfg.Subagent.Enabled {
				reg.Register(subagent.NewSpawnTool(subResolver, reg, subFactory, cfg.Subagent.MaxDepth).
					WithBudget(cfg.Subagent.MaxTotal, cfg.Subagent.MaxConcurrent))
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
				for _, t := range full.All() {
					if allow[t.Name()] {
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

		handler := chatapi.NewHandler(newChatLoop, baseSystem).
			WithRuntime(sessionRuntime).
			WithMessageStore(messageStore).
			WithContextBuilder(ctxBuilder).
			WithTeamAttributor(teamAttributor).
			WithBudgetGate(chatapi.BudgetChecker(budgetGate))
		if imageStore != nil {
			handler = handler.WithImageStore(imageStore)
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
		if sandboxMgr != nil {
			log.Info("file tools enabled (read_file/write_file/list_dir/edit_file/grep/glob/move_file/copy_file/delete_file/make_dir)")
		}
		if execEnabled {
			log.Info("run_command tool enabled", "backend", cfg.Sandbox.Backend)
		}
		// MCP tool count is logged by reconnectMCP when the async connect lands;
		// at this point it is still 0, so there is nothing to report.
		if cfg.Subagent.Enabled {
			log.Info("subagent tool enabled (spawn_agent)", "max_depth", cfg.Subagent.MaxDepth)
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
		// firing (scheduled sweep and run-now) needs a provider.
		if cfg.Schedule.Enabled {
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
			trigger := schedule.NewTrigger(schedStore, sessionRuntime, handler.Registry(), subResolver.Bound(ctx), identitySvc, buildSchedLoop, pool, cfg.Schedule.ScanInterval)
			// Tool binder: narrow the session's tool registry to the task's
			// whitelist (D3) once the trigger has resolved the session id.
			trigger.WithToolBinder(func(ctx context.Context, loop *agent.Loop, sessionID string, whitelist []string) {
				loop.WithTools(buildToolRegistry(ctx, sessionID, whitelist))
			})
			trigger.WithTeamAttributor(teamAttributor)
			trigger.WithBudgetGate(schedule.BudgetChecker(budgetGate))
			trigger.SetLogger(log)
			go trigger.Start(ctx)
			schedTrigger = trigger
			log.Info("scheduled-task trigger enabled", "scan_interval", cfg.Schedule.ScanInterval)
		} else {
			log.Info("scheduled-task trigger disabled; task CRUD still available")
		}
		log.Info("chat endpoint enabled (auth required); provider+model resolved per request from the registry")
	}

	// Management console (admin-console): self-service, team, and platform
	// routes, all behind the same auth middleware the chat endpoint uses. It is
	// registered outside the provider branch so the console stays reachable on
	// a deployment with no provider configured.
	adminHandler := adminapi.NewHandler(identitySvc, usage.NewStore(pool), memPort).
		WithQuotas(quota.NewStore(pool)).
		WithProviders(provStore).
		WithUploads(uploadSvc).
		WithDreaming(dreamRunner).
		WithAudit(auditLogger)
	adminHandler.RegisterAuthed(protected)
	log.Info("admin console endpoints enabled (auth required)")

	// Skill management (skill-console): user/team/system skill CRUD + versioning,
	// behind the same auth middleware. Registered alongside the admin console.
	skillapi.NewHandler(identitySvc, skillStore).RegisterAuthed(protected)
	log.Info("skill management endpoints enabled (auth required)")

	// Agent-definition management (persist-agent-defs): user/team/system
	// definition CRUD over the same PG store the spawn resolver reads, behind
	// the same auth middleware. The runnable check mirrors the run registry's
	// run_skill_script registration rule (exec enabled + some visible skill has
	// scripts), so a definition declaring unusable skills is flagged on write.
	skillsRunnable := func(ctx context.Context, scopes []identity.ScopeRef) bool {
		if !execEnabled {
			return false
		}
		l0, err := skillEngine.LoadL0(ctx, scopes)
		if err != nil {
			return false
		}
		for _, meta := range l0 {
			if len(meta.Scripts) > 0 {
				return true
			}
		}
		return false
	}
	agentdefapi.NewHandler(identitySvc, agentDefPG, skillsRunnable).RegisterAuthed(protected)
	log.Info("agent definition endpoints enabled (auth required)")

	// Scheduled-task CRUD (scheduled-tasks): self-service management of recurring
	// agent runs. Registered outside the provider branch so tasks can be managed
	// on a deployment with no LLM; only firing needs a provider, so run-now is
	// wired to the trigger when one was built above and answers 503 otherwise.
	scheduleapi.NewHandler(schedule.NewPGStore(pool)).WithRunner(schedTrigger).RegisterAuthed(protected)
	log.Info("scheduled-task endpoints enabled (auth required)")

	// Mount the protected tier once: every authed route above is now behind the
	// group's middleware set, with a single wrap instead of one per route.
	protected.Mount(mux, "/api/")

	// Serve the built frontend if present. The console is a client-side route,
	// so a deep link like /admin/users has no file behind it — spaHandler falls
	// back to index.html. API routes carry more specific patterns, which Go's
	// ServeMux prefers over this one, so they are unaffected.
	if cfg.Web.Dir != "" {
		// SPA must be registered method-less: "GET /" would conflict with the
		// all-methods "/api/" subtree (more general path, fewer methods). The
		// method guard lives in spaHandler instead.
		mux.Handle("/", spaHandler(cfg.Web.Dir))
	}

	srv := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      httpHandler(cfg, log, metrics, mux),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTP.Addr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

// reconnectMCP keeps trying to establish the MCP session until it succeeds or
// the server shuts down. The first failure is logged as a warning so an
// unreachable/misconfigured server is visible; subsequent retries back off
// quietly and success is announced once tools are registered. It runs for the
// process lifetime (ctx is the signal context) and only stops on shutdown.
func reconnectMCP(ctx context.Context, c *mcp.Client, log *slog.Logger) {
	backoff := time.Second
	first := true
	for {
		if err := c.Connect(ctx); err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			if first {
				log.Warn("mcp connect failed; retrying in background", "server", c.Server(), "err", err)
				first = false
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		log.Info("mcp connected", "server", c.Server(), "tools", len(c.Tools()))
		return
	}
}

// noProviderAdapter fails every generation with the resolver's ErrNoProvider.
// It is the loop's adapter for a chat request that could not be resolved (empty
// or misconfigured registry), so the client receives a clean error frame
// instead of a hang or a panic — the chatapi LoopFactory cannot surface an
// error, so the loop carries the failure instead.
type noProviderAdapter struct{}

func (noProviderAdapter) Name() string { return "none" }
func (noProviderAdapter) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	return nil, providerreg.ErrNoProvider
}

// spaHandler serves static files from dir, falling back to index.html for
// paths that do not name a file. That fallback is what makes client-side routes
// (/admin/users and friends) survive a reload or a shared link — a plain
// FileServer answers 404 for them.
//
// The fallback deliberately does NOT apply to /api/: those routes are
// registered with more specific patterns, which Go 1.22+ ServeMux matches in
// preference to "GET /". A request that reaches here asking for a missing asset
// (a stale .js hash, say) gets index.html rather than a 404, which is the
// standard SPA trade-off — the alternative is enumerating asset extensions.
func spaHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		// Reject traversal before touching the filesystem: path.Clean on a
		// rooted path cannot escape, and http.ServeFile would reject it anyway,
		// but checking here keeps the stat below honest.
		clean := path.Clean("/" + r.URL.Path)
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clean))); err == nil && clean != "/" {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, index)
	})
}

// httpHandler composes the inbound middleware stack around the mux via Chain,
// outermost first. Order invariants:
//
//   - request-id outermost: an id is near-free (one random read), and giving
//     EVERY request one — including requests the limiter is about to reject —
//     keeps throttled floods traceable instead of anonymous.
//   - access-log next: one line per completed request (status/latency/ttfb/
//     bytes, 499 on client disconnect), so rejected requests are also seen.
//   - rate-limit (P1-1) before metrics: a flood must not churn metric series;
//     probes are opted out so monitoring stays up during a flood. Limiting is
//     disabled unless both HTTP_RATE_LIMIT_RPS and _BURST are set.
//   - metrics then recovery, innermost around the mux: a panic is recovered,
//     logged with a stack, and answered 500 — and because recovery sits inside
//     metrics, that 500 is counted like any other status.
func httpHandler(cfg config.Config, log *slog.Logger, metrics *observability.Metrics, mux *http.ServeMux) http.Handler {
	limiter := quota.NewRateLimiter(cfg.HTTP.RateLimitRPS, cfg.HTTP.RateLimitBurst,
		func(r *http.Request) string {
			// Never throttle probes: a flooded API must not blind the operator.
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				return ""
			}
			return quota.ClientIPKey(r)
		})
	return observability.StandardStack(mux, log, metrics, limiter.Middleware)
}

// buildEncryptor constructs the secret encryptor from config, or nil when no
// master key is set (encryption disabled, plaintext fallback). An error means a
// key WAS provided but is malformed — that is a hard failure, because silently
// ignoring a mis-set SECRETS_MASTER_KEY would boot with keys unprotected while
// the operator believes they are encrypted.
func buildEncryptor(cfg config.Config) (*secrets.Encryptor, error) {
	if cfg.Secrets.MasterKey == "" {
		return nil, nil
	}
	return secrets.NewSingle([]byte(cfg.Secrets.MasterKey))
}
