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
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/logging"
	"nowhere-agent/internal/platform/db"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/provider/anthropic"
	"nowhere-agent/internal/provider/openai"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
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

	// Chat endpoint: build an agent loop per request from the configured provider.
	if adapter := buildProvider(cfg, log); adapter != nil {
		model := cfg.LLM.Model
		chatapi.NewHandler(func(ctx context.Context) *agent.Loop {
			return agent.New(adapter, toolruntime.NewRegistry(), agent.Config{
				Model:           model,
				MaxTokens:       4096,
				MaxIterations:   25,
				CacheablePrefix: true,
			})
		}, "").WithRuntime(sessionRuntime).RegisterAuthed(mux, identityHandler.RequireAuth)
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
	switch cfg.LLM.Provider {
	case "anthropic":
		var opts []anthropic.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, anthropic.WithEndpoint(cfg.LLM.BaseURL))
		}
		return anthropic.New(cfg.LLM.APIKey, opts...)
	case "openai":
		var opts []openai.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, openai.WithEndpoint(cfg.LLM.BaseURL))
		}
		return openai.New(cfg.LLM.APIKey, opts...)
	default:
		return nil
	}
}
