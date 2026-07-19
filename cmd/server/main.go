// Command server runs the nowhere-agent gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/config"
	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/logging"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/platform/db"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/provider/anthropic"
	"nowhere-agent/internal/provider/openai"
	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/skill"
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
	sessionRuntime := session.NewRuntime(session.NewPGStore(pool))

	// Live content broker (redis-stream-live): in-memory for single instance,
	// Redis Streams for multi-instance. Selected via STREAM_BROKER; a redis
	// broker that is unreachable at boot fails fast (a multi-instance deploy
	// with a dead broker is a misconfiguration worth surfacing).
	if cfg.Stream.Broker == "redis" {
		broker := session.NewRedisBroker(cfg.Stream.RedisAddr, 0, 0)
		if err := session.PingRedis(ctx, cfg.Stream.RedisAddr); err != nil {
			return fmt.Errorf("stream broker redis at %s: %w", cfg.Stream.RedisAddr, err)
		}
		sessionRuntime = sessionRuntime.WithBroker(broker)
		log.Info("live content broker: redis streams", "addr", cfg.Stream.RedisAddr)
	} else {
		log.Info("live content broker: in-memory (single instance)")
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
		sandboxPort = sandbox.NewLocalPort(root)
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

	// Memory (PG+vector) and skill engine feed the loop's system prompt:
	// L0 skill index + recalled memories, scoped to the caller (task 4.5).
	memPort := memory.NewPGPort(pool)
	skillEngine := skill.NewEngine(skill.NewStore())
	baseSystem := "You are nowhere-agent, a helpful AI assistant."
	ctxBuilder := chatapi.NewContextBuilder(baseSystem, identitySvc, memPort, skillEngine)

	// Chat endpoint: build an agent loop per request from the configured provider.
	if adapter := buildProvider(cfg, log); adapter != nil {
		model := cfg.LLM.Model
		// Context compression (context-compression): the loop compresses its
		// working view as it approaches the model's context window, using a
		// no-tools summarize call over the same adapter (LLMCompressor).
		var compressor contextmgmt.Compressor
		if cfg.LLM.ContextWindow > 0 {
			compressor = contextmgmt.NewLLMCompressor(adapter, model)
		}
		handler := chatapi.NewHandler(func(ctx context.Context, system string) *agent.Loop {
			return agent.New(adapter, toolruntime.NewRegistry(), agent.Config{
				Model:           model,
				System:          system,
				MaxTokens:       4096,
				MaxIterations:   25,
				CacheablePrefix: true,
				ContextWindow:   cfg.LLM.ContextWindow,
				Compressor:      compressor,
			})
		}, baseSystem).
			WithRuntime(sessionRuntime).
			WithMessageStore(messageStore).
			WithContextBuilder(ctxBuilder)
		if imageStore != nil {
			handler = handler.WithImageStore(imageStore)
		}
		if sandboxMgr != nil {
			handler = handler.WithToolBinder(func(ctx context.Context, loop *agent.Loop, sessionID string) {
				h, err := sandboxMgr.Ensure(ctx, sessionID, sandbox.Options{})
				if err != nil {
					log.Warn("sandbox ensure failed; run has no file tools", "session", sessionID, "err", err)
					return
				}
				reg := toolruntime.NewRegistry()
				for _, t := range builtin.FileTools(sandboxPort, h) {
					reg.Register(t)
				}
				loop.WithTools(reg)
			})
			log.Info("file tools enabled (read_file/write_file/list_dir)")
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
