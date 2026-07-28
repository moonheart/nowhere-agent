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
	"syscall"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/config"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/logging"
	"nowhere-agent/internal/mcp"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/permission"
	"nowhere-agent/internal/platform/db"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/provider/anthropic"
	"nowhere-agent/internal/provider/openai"
	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/scheduler"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/skill"
	"nowhere-agent/internal/subagent"
	"nowhere-agent/internal/toolruntime"
	"nowhere-agent/internal/toolruntime/builtin"
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	identityStore := identity.NewStore(pool)
	identitySvc := identity.NewService(identityStore)
	identityHandler := identity.NewHandler(identitySvc)
	identityHandler.Register(mux)

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
	skillStore := skill.NewStore()
	// Seed the skill store from disk (capability-gap K3a): each SKILL.md under
	// SKILLS_DIR becomes a system-scope skill, lighting up the L0 index in the
	// system prompt. Empty SKILLS_DIR leaves the runtime dormant. The loader
	// never loads scripts — skill execution is K3b, gated on C17.
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

	// Chat endpoint: build an agent loop per request from the configured provider.
	if adapter := buildProvider(cfg, log); adapter != nil {
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
		permit := func(t toolruntime.Tool) (bool, string) {
			switch permChecker.Check(t) {
			case permission.Deny:
				return true, fmt.Sprintf("%s (risk: %s) is not permitted by policy", t.Name(), t.Risk())
			case permission.Ask:
				// Gate for human approval (capability-gap O2): the marker tells the
				// loop to SUSPEND the run and prompt the user, not feed an error to
				// the model. The decision endpoint resumes the run.
				return true, agent.ApprovalReasonPrefix + fmt.Sprintf("%s (risk: %s)", t.Name(), t.Risk())
			default:
				return false, ""
			}
		}
		log.Info("execution-permission gate enabled",
			"read_only", cfg.Permission.ReadOnly, "sandbox_write", cfg.Permission.SandboxWrite,
			"network", cfg.Permission.Network, "external_write", cfg.Permission.ExternalWrite)

		// MCP integration (mcp capability): connect to the configured SearXNG MCP
		// server over Streamable HTTP and list its tools once. The client is shared
		// across runs; the ToolBinder registers its tools into each run's registry
		// so subagents inherit them via the scoped view. Fail fast on a handshake
		// error — enabling MCP against an unreachable server is a misconfiguration.
		var mcpClient *mcp.Client
		if cfg.MCP.Enabled {
			mcpClient = mcp.NewSearxng(cfg.MCP.SearxngURL, 0)
			if err := mcpClient.Connect(ctx); err != nil {
				return fmt.Errorf("mcp searxng connect: %w", err)
			}
			log.Info("mcp searxng connected", "url", cfg.MCP.SearxngURL, "tools", len(mcpClient.Tools()))
		}
		// Context compression (context-compression): the loop compresses its
		// working view as it approaches the model's context window, using a
		// no-tools summarize call over the same adapter (LLMCompressor).
		var compressor contextmgmt.Compressor
		if cfg.LLM.ContextWindow > 0 {
			compressor = contextmgmt.NewLLMCompressor(adapter, model)
		}

		// Dreaming worker + the scheduler that drives it (capability-gaps K1+K2).
		// The worker consolidates ended sessions' episodes into long-term memory;
		// the scheduler fires it every DREAMING_INTERVAL. Idempotency rests on the
		// sessions.dreamed_at marker (migration 000008), not the scheduler's
		// in-memory last-run map, so the catch-up run at every boot only processes
		// sessions not already dreamed over. The scheduler runs in a goroutine like
		// the HTTP server and stops when the root context is cancelled.
		if cfg.Dreaming.Enabled {
			source := dreaming.NewStoreSource(sessionStore, messageStore)
			llm := dreaming.NewProviderLLM(adapter, model)
			worker := dreaming.NewWorker(source, memPort, llm, dreaming.Budget{MaxTokens: cfg.Dreaming.MaxTokens})
			worker.SetReflect(cfg.Dreaming.Reflect)
			worker.SetRevise(cfg.Dreaming.Revise)
			sched := scheduler.New(log, scheduler.Job{
				Name:     "dreaming",
				Interval: cfg.Dreaming.Interval,
				Run: func(ctx context.Context) error {
					_, err := worker.Run(ctx)
					return err
				},
			})
			go sched.Start(ctx)
			log.Info("dreaming worker enabled", "interval", cfg.Dreaming.Interval, "max_tokens", cfg.Dreaming.MaxTokens, "reflect", cfg.Dreaming.Reflect, "revise", cfg.Dreaming.Revise)
		}

		// Subagent factory (subagent capability): builds a child loop for a
		// resolved definition. System prompt and model come from the definition
		// (model falls back to the parent's); the child's tool registry is set by
		// the spawn tool via WithTools. Closes over the provider so the subagent
		// package needs no wiring dependency.
		subStore := agentdef.NewStore()
		subFactory := func(_ context.Context, def agentdef.AgentDef, _ int) *agent.Loop {
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
				MaxTokens:       4096,
				MaxIterations:   maxIter,
				CacheablePrefix: true,
				Permission:      permit,
			})
			// Cross-cutting middleware, outermost first: compression shrinks the
			// working view, overflow retry drops a round and retries on rejection.
			if compressor != nil {
				loop.Use(&agent.CompressMW{Compressor: compressor, Window: cfg.LLM.ContextWindow, MaxTokens: 4096})
			}
			loop.Use(&agent.OverflowMW{})
			return loop
		}

		// Loop factory + session tool binder, named so the approval Resume path
		// can rebuild a parked run's loop after a restart (capability-gap O2).
		newChatLoop := func(ctx context.Context, system string) *agent.Loop {
			loop := agent.New(adapter, toolruntime.NewRegistry(), agent.Config{
				Model:           model,
				System:          system,
				MaxTokens:       4096,
				MaxIterations:   25,
				CacheablePrefix: true,
				Permission:      permit,
			})
			if compressor != nil {
				loop.Use(&agent.CompressMW{Compressor: compressor, Window: cfg.LLM.ContextWindow, MaxTokens: 4096})
			}
			loop.Use(&agent.OverflowMW{})
			return loop
		}
		bindChatTools := func(ctx context.Context, loop *agent.Loop, sessionID string) {
			reg := toolruntime.NewRegistry()
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
			// builder (caller user + teams + system). It executes nothing — skill
			// script execution is K3b, gated on C17.
			if len(skillEngine.LoadL0(ctx, nil)) > 0 {
				scopes := []identity.ScopeRef{identity.SystemScope()}
				if sess, err := sessionRuntime.GetSession(ctx, sessionID); err == nil {
					if sc, err := identitySvc.AccessibleScopes(ctx, sess.UserID); err == nil {
						scopes = sc
					}
				}
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
			loop.WithTools(reg)
		}

		handler := chatapi.NewHandler(newChatLoop, baseSystem).
			WithRuntime(sessionRuntime).
			WithMessageStore(messageStore).
			WithContextBuilder(ctxBuilder)
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
			if mcpClient != nil {
				log.Info("mcp tools enabled", "server", mcpClient.Server(), "count", len(mcpClient.Tools()))
			}
			if cfg.Subagent.Enabled {
				log.Info("subagent tool enabled (spawn_agent)", "max_depth", cfg.Subagent.MaxDepth)
			}
		}
		handler.RegisterAuthed(mux, identityHandler.RequireAuth)
		if compressor != nil {
			log.Info("context compression enabled", "window", cfg.LLM.ContextWindow)
		}
		log.Info("chat endpoint enabled (auth required)", "provider", adapter.Name(), "model", model)
	} else {
		log.Warn("chat endpoint disabled: no LLM provider configured (set LLM_PROVIDER/LLM_API_KEY)")
	}

	// Serve the built frontend if present.
	if cfg.Web.Dir != "" {
		mux.Handle("GET /", http.FileServer(http.Dir(cfg.Web.Dir)))
	}

	srv := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      mux,
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

// buildProvider constructs the configured provider adapter, or nil if not configured.
func buildProvider(cfg config.Config, log *slog.Logger) provider.Adapter {
	recorder := provider.NewRawRecorder(cfg.LLM.RawLogDir)
	if recorder.Enabled() {
		log.Info("recording raw LLM request/response", "dir", cfg.LLM.RawLogDir)
	}
	switch cfg.LLM.Provider {
	case "anthropic":
		var opts []anthropic.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, anthropic.WithEndpoint(cfg.LLM.BaseURL))
		}
		opts = append(opts, anthropic.WithRawRecorder(recorder))
		return anthropic.New(cfg.LLM.APIKey, opts...)
	case "openai":
		var opts []openai.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, openai.WithEndpoint(cfg.LLM.BaseURL))
		}
		opts = append(opts, openai.WithRawRecorder(recorder))
		return openai.New(cfg.LLM.APIKey, opts...)
	default:
		return nil
	}
}
