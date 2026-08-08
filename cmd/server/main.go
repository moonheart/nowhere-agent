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
	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/config"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/logging"
	"nowhere-agent/internal/mcp"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/observability"
	"nowhere-agent/internal/oidc"
	"nowhere-agent/internal/permission"
	"nowhere-agent/internal/platform/db"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/provider/anthropic"
	"nowhere-agent/internal/provider/openai"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/routing"
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
	"nowhere-agent/internal/usage"
	"nowhere-agent/internal/workspace"
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

	// Team-scoped provider credentials (model-routing D14). The store is both
	// the resolver on the chat path (below) and the console's management path.
	// Keys are encrypted at rest (enterprise-readiness P0-2) when a master key is
	// configured; without one the store falls back to plaintext and we say so,
	// because a deployment storing real provider credentials unprotected should
	// not do so silently.
	keyStore := routing.NewPGKeyStore(pool, cfg.LLM.APIKey)
	if enc, err := buildEncryptor(cfg); err != nil {
		return fmt.Errorf("secrets: %w", err)
	} else if enc != nil {
		keyStore.WithEncryption(enc)
		log.Info("team provider keys encrypted at rest (AES-256-GCM)")
	} else {
		log.Warn("SECRETS_MASTER_KEY unset: team provider keys stored PLAINTEXT; set it to enable encryption at rest")
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

	// Full-block conversation record (persist-raw-messages): messages are
	// persisted in original form and cross-run history is rebuilt from it.
	messageStore := session.NewPGMessageStore(pool)

	// Workspace image store: image payloads referenced by messages live as
	// WebP files under a per-session dir; the messages table holds pointers.
	var imageStore *workspace.ImageStore
	if cfg.Workspace.Dir != "" {
		imageStore = workspace.NewImageStore(cfg.Workspace.Dir)
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
	// team whose provider key pays for it, so per-team cost reports read the run
	// row directly. Resolution mirrors the credential lookup: a hiccup yields ""
	// (platform-billed), never a blocked run. Shared by the chat handler and the
	// scheduled-task trigger, which attributes the same way as a human run.
	teamAttributor := func(ctx context.Context, userID string) string {
		creds, err := keyStore.Resolve(ctx, userID, cfg.LLM.Provider)
		if err != nil {
			return ""
		}
		return creds.TeamID
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

	// Chat endpoint: build an agent loop per request from the configured provider.
	if adapter, rawRecorder := buildProvider(cfg, log); adapter != nil {
		model := cfg.LLM.Model

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
		// permitEnv evaluates the env policy: deny → blocked (fed to the model);
		// ask → gated for human approval (the ApprovalReasonPrefix marker tells the
		// loop to SUSPEND and prompt, not error). This is the base policy the
		// per-session mode wraps.
		permitEnv := func(t toolruntime.Tool) (bool, string) {
			switch permChecker.Check(t) {
			case permission.Deny:
				return true, fmt.Sprintf("%s (risk: %s) is not permitted by policy", t.Name(), t.Risk())
			case permission.Ask:
				return true, agent.ApprovalReasonPrefix + fmt.Sprintf("%s (risk: %s)", t.Name(), t.Risk())
			default:
				return false, ""
			}
		}
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
		// permit is the GateFunc the PermissionMW middleware exposes to the loop,
		// registered ONCE per loop. The loop calls it at both gate points on every
		// tool call, so resolving the mode HERE (per call, from the run context's
		// session id) — not at registration time — makes the client's "allow all"
		// toggle take effect with no loop rebuild and no middleware re-wiring, and
		// lets a subagent child inherit its parent session's mode through the same
		// context. allow_all lifts ONLY the approval gate (the ask marker): an env
		// deny still blocks, and ask_user/client_tool are unaffected. The mode read
		// is best-effort: any failure or unknown value falls back to auto (env).
		permit := func(ctx context.Context, t toolruntime.Tool) (bool, string) {
			deny, reason := permitEnv(t)
			if deny && agent.IsApprovalReason(reason) && permissionMode(ctx, agent.SessionIDFromContext(ctx)) == chatapi.PermissionModeAllowAll {
				return false, ""
			}
			return deny, reason
		}
		log.Info("execution-permission gate enabled",
			"read_only", cfg.Permission.ReadOnly, "sandbox_write", cfg.Permission.SandboxWrite,
			"network", cfg.Permission.Network, "external_write", cfg.Permission.ExternalWrite)

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
		compressionEnabled := cfg.LLM.ContextWindow > 0

		// replyBudget reserves response space inside the context window. With a
		// small window configured, the 64k default would exceed it (the
		// provider can reject max_tokens beyond the window) and leave the
		// compression budget (window - reply) negative, so it is clamped to a
		// quarter of the window.
		replyBudget := 65536
		if cfg.LLM.ContextWindow > 0 && cfg.LLM.ContextWindow/4 < replyBudget {
			replyBudget = cfg.LLM.ContextWindow / 4
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
		source := dreaming.NewStoreSource(sessionStore, messageStore)
		worker := dreaming.NewWorker(source, memPort,
			dreaming.NewProviderLLM(adapter, model),
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

		// Subagent factory (subagent capability): builds a child loop for a
		// resolved definition. System prompt and model come from the definition
		// (model falls back to the parent's); the child's tool registry is set by
		// the spawn tool via WithTools. Closes over the provider so the subagent
		// package needs no wiring dependency.
		subStore := agentdef.NewStore()
		subFactory := func(ctx context.Context, def agentdef.AgentDef, _ int) *agent.Loop {
			childModel := def.Model
			if childModel == "" {
				childModel = model
			}
			maxIter := 25
			if def.MaxTurns > 0 {
				maxIter = def.MaxTurns
			}
			loop := agent.New(adapter, toolruntime.NewRegistry(), agent.Config{
				Model:           childModel,
				System:          def.System,
				MaxTokens:       replyBudget,
				MaxIterations:   maxIter,
				CacheablePrefix: true,
			})
			// The child's permission policy resolves from the spawn context's session
			// id (set on the run by the registry), so it inherits the parent session's
			// permission mode.
			loop.Use(&agent.PermissionMW{Check: permit})
			if compressionEnabled {
				loop.Use(&agent.CompressMW{Compressor: contextmgmt.NewLLMCompressor(adapter, childModel), Window: cfg.LLM.ContextWindow, MaxTokens: replyBudget})
			}
			loop.Use(&agent.OverflowMW{})
			return loop
		}

		// Loop factory + session tool binder, named so the approval Resume path
		// can rebuild a parked run's loop after a restart (capability-gap O2).
		//
		// Credential resolution happens here, per request (model-routing): the
		// caller is already on the context (both call sites pass the request
		// context from a route behind RequireAuth), so a team that configured
		// its own provider key gets its calls billed to that key instead of the
		// platform one. Resolution failure falls back to the boot adapter —
		// chat must not go down because a key lookup hiccuped.
		// newChatLoopWithModel builds the chat loop for an explicit model. It is
		// the core both the chat path and the scheduled-task trigger use; the
		// trigger passes the task's model (falling back to the chat default).
		newChatLoopWithModel := func(ctx context.Context, system, modelOverride string) *agent.Loop {
			m := model
			if modelOverride != "" {
				m = modelOverride
			}
			callerAdapter := adapterForCaller(ctx, cfg, rawRecorder, keyStore, adapter, log)
			loop := agent.New(
				callerAdapter,
				toolruntime.NewRegistry(), agent.Config{
					Model:           m,
					System:          system,
					MaxTokens:       replyBudget,
					MaxIterations:   25,
					CacheablePrefix: true,
				})
			// Tool authorization gates dispatch. The policy (permit) resolves the
			// per-session permission mode from the run context at call time, so one
			// registration covers every session and reacts to the live toggle.
			loop.Use(&agent.PermissionMW{Check: permit})
			if compressionEnabled {
				loop.Use(&agent.CompressMW{Compressor: contextmgmt.NewLLMCompressor(callerAdapter, m), Window: cfg.LLM.ContextWindow, MaxTokens: replyBudget})
			}
			loop.Use(&agent.OverflowMW{})
			return loop
		}
		newChatLoop := func(ctx context.Context, system string) *agent.Loop {
			return newChatLoopWithModel(ctx, system, "")
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
			// Subagent spawn tool: children draw from a scoped view of this run's
			// registry, so nested loops share the session's tools. Registered last.
			if cfg.Subagent.Enabled {
				reg.Register(subagent.NewSpawnTool(subStore, reg, subFactory, cfg.Subagent.MaxDepth).
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
		// Incremental memory injection (capability K / context-mgmt): each run's
		// loop surfaces newly-created memories into the outgoing view (never the
		// durable history), keeping the system prefix byte-stable for caching.
		handler = handler.WithMemoryInjector(func(ctx context.Context, user identity.User, query string) agent.MemoryInjector {
			return chatapi.NewSessionMemoryInjector(memPort, identitySvc, sessionRuntime, user, query)
		})
		// Tool binder: attach session-scoped tools to each run. Runs when the
		// sandbox (file tools), MCP (network tools), or memory (recall_memory) is
		// configured — the latter two need no sandbox, so they must register even
		// when the sandbox is off.
		if sandboxMgr != nil || mcpClient != nil || memPort != nil {
			handler = handler.WithToolBinder(bindChatTools)
			if sandboxMgr != nil {
				log.Info("file tools enabled (read_file/write_file/list_dir/edit_file/grep/glob/move_file/copy_file/delete_file/make_dir)")
			}
			if execEnabled {
				log.Info("run_command tool enabled", "backend", cfg.Sandbox.Backend)
			}
			// MCP tool count is logged by reconnectMCP when the async connect
			// lands; at this point it is still 0, so there is nothing to report.
			if cfg.Subagent.Enabled {
				log.Info("subagent tool enabled (spawn_agent)", "max_depth", cfg.Subagent.MaxDepth)
			}
		}
		handler.RegisterAuthed(mux, identityHandler.RequireAuth)

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
			// model (modelOverride "" = the chat default). Tools are NOT bound here
			// — the target session is not yet known — but via WithToolBinder once
			// the trigger resolves it.
			buildSchedLoop := func(ctx context.Context, task schedule.Task, system, modelOverride string) *agent.Loop {
				return newChatLoopWithModel(ctx, system, modelOverride)
			}
			trigger := schedule.NewTrigger(schedStore, sessionRuntime, handler.Registry(), subStore, identitySvc, buildSchedLoop, pool, cfg.Schedule.ScanInterval)
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
		if compressionEnabled {
			log.Info("context compression enabled", "window", cfg.LLM.ContextWindow)
		}
		log.Info("chat endpoint enabled (auth required)", "provider", adapter.Name(), "model", model)
	} else {
		log.Warn("chat endpoint disabled: no LLM provider configured (set LLM_PROVIDER/LLM_API_KEY)")
	}

	// Management console (admin-console): self-service, team, and platform
	// routes, all behind the same auth middleware the chat endpoint uses. It is
	// registered outside the provider branch so the console stays reachable on
	// a deployment with no LLM configured.
	adminHandler := adminapi.NewHandler(identitySvc, keyStore, usage.NewStore(pool), memPort).
		WithQuotas(quota.NewStore(pool)).
		WithDreaming(dreamRunner).
		WithAudit(auditLogger)
	adminHandler.RegisterAuthed(mux, identityHandler.RequireAuth)
	log.Info("admin console endpoints enabled (auth required)")

	// Skill management (skill-console): user/team/system skill CRUD + versioning,
	// behind the same auth middleware. Registered alongside the admin console.
	skillapi.NewHandler(identitySvc, skillStore).RegisterAuthed(mux, identityHandler.RequireAuth)
	log.Info("skill management endpoints enabled (auth required)")

	// Scheduled-task CRUD (scheduled-tasks): self-service management of recurring
	// agent runs. Registered outside the provider branch so tasks can be managed
	// on a deployment with no LLM; only firing needs a provider, so run-now is
	// wired to the trigger when one was built above and answers 503 otherwise.
	scheduleapi.NewHandler(schedule.NewPGStore(pool)).WithRunner(schedTrigger).RegisterAuthed(mux, identityHandler.RequireAuth)
	log.Info("scheduled-task endpoints enabled (auth required)")

	// Serve the built frontend if present. The console is a client-side route,
	// so a deep link like /admin/users has no file behind it — spaHandler falls
	// back to index.html. API routes carry more specific patterns, which Go's
	// ServeMux prefers over this one, so they are unaffected.
	if cfg.Web.Dir != "" {
		mux.Handle("GET /", spaHandler(cfg.Web.Dir))
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

// adapterForCaller picks the provider adapter for one chat request
// (model-routing): the platform adapter, unless the caller belongs to a team
// that configured its own key for this provider, in which case an adapter bound
// to that key.
//
// Every failure path returns the platform adapter. That is the whole point:
// this runs on the chat hot path, and a credential lookup that errors — a
// Postgres blip, a misconfigured row — must degrade to the platform key rather
// than take chat down. An unauthenticated context does the same, which is what
// makes this safe to call from paths that have no user.
func adapterForCaller(
	ctx context.Context,
	cfg config.Config,
	recorder *provider.RawRecorder,
	keys *routing.PGKeyStore,
	platform provider.Adapter,
	log *slog.Logger,
) provider.Adapter {
	if keys == nil {
		return platform
	}
	u, ok := identity.UserFromContext(ctx)
	if !ok {
		return platform
	}
	creds, err := keys.Resolve(ctx, u.ID, cfg.LLM.Provider)
	if err != nil {
		log.Warn("credential resolution failed; using platform key", "user", u.ID, "err", err)
		return platform
	}
	if creds.Platform || creds.APIKey == "" {
		return platform
	}
	teamAdapter := buildProviderWithKey(cfg, recorder, creds.APIKey)
	if teamAdapter == nil {
		return platform
	}
	return teamAdapter
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

// httpHandler composes the inbound middleware stack around the mux, outermost
// first: rate-limit (P1-1, reject a flood before any per-request work or metric
// is spent on it) → request-id (so every logged/served request carries an id) →
// metrics (count what actually serves) → mux. Health and metrics probes are
// opted out of rate limiting so monitoring stays up during a flood. Limiting is
// disabled unless both HTTP_RATE_LIMIT_RPS and _BURST are set.
func httpHandler(cfg config.Config, log *slog.Logger, metrics *observability.Metrics, mux *http.ServeMux) http.Handler {
	inner := observability.RequestID(log)(metrics.Middleware(mux))
	limiter := quota.NewRateLimiter(cfg.HTTP.RateLimitRPS, cfg.HTTP.RateLimitBurst,
		func(r *http.Request) string {
			// Never throttle probes: a flooded API must not blind the operator.
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				return ""
			}
			return quota.ClientIPKey(r)
		})
	return limiter.Middleware(inner)
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

// buildProvider constructs the configured provider adapter from the platform
// key, or nil if not configured. It also returns the raw recorder so the
// per-request adapters (built for team keys) share one rather than each opening
// its own log directory handle.
func buildProvider(cfg config.Config, log *slog.Logger) (provider.Adapter, *provider.RawRecorder) {
	recorder := provider.NewRawRecorder(cfg.LLM.RawLogDir)
	if recorder.Enabled() {
		log.Info("recording raw LLM request/response", "dir", cfg.LLM.RawLogDir)
	}
	return buildProviderWithKey(cfg, recorder, cfg.LLM.APIKey), recorder
}

// buildProviderWithKey constructs an adapter for a specific API key. It is how
// a team-configured credential becomes a usable adapter on the request path
// (model-routing). Adapters hold the shared http.DefaultClient and a few
// fields, so constructing one per request is a struct literal — connection
// pooling is preserved through the shared client, and no adapter cache is
// warranted.
func buildProviderWithKey(cfg config.Config, recorder *provider.RawRecorder, apiKey string) provider.Adapter {
	switch cfg.LLM.Provider {
	case "anthropic":
		var opts []anthropic.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, anthropic.WithEndpoint(cfg.LLM.BaseURL))
		}
		opts = append(opts, anthropic.WithRawRecorder(recorder))
		return anthropic.New(apiKey, opts...)
	case "openai":
		var opts []openai.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, openai.WithEndpoint(cfg.LLM.BaseURL))
		}
		opts = append(opts, openai.WithRawRecorder(recorder))
		return openai.New(apiKey, opts...)
	default:
		return nil
	}
}
