// Command smoketest calls the resolved LLM endpoint once with a trivial prompt
// and prints the streamed canonical events. It resolves provider+model from the
// Postgres registry — the platform default, or a team's assignment when
// SMOKE_TEAM_ID is set. Used to verify the provider adapter against a live
// model — not part of the test suite.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/config"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/secrets"
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

	db, err := sql.Open("pgx", cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	store := providerreg.NewPGStore(db)
	if cfg.Secrets.MasterKey != "" {
		enc, err := secrets.NewSingle([]byte(cfg.Secrets.MasterKey))
		if err != nil {
			return err
		}
		store = store.WithEncryption(enc)
	}
	resolver := providerreg.NewResolver(store)
	recorder := provider.NewRawRecorder(cfg.LLM.RawLogDir)

	ctx := context.Background()
	teamID := os.Getenv("SMOKE_TEAM_ID")
	var target providerreg.Target
	if teamID != "" {
		target, err = resolver.ResolveForTeam(ctx, teamID)
	} else {
		target, err = resolver.Resolve(ctx, os.Getenv("SMOKE_USER_ID"))
	}
	if err != nil {
		return fmt.Errorf("resolve provider: %w (is the DB migrated and a provider configured?)", err)
	}

	adapter := providerreg.BuildAdapter(target, recorder, cfg.LLM.StreamIdleTimeout)
	if adapter == nil {
		return fmt.Errorf("unsupported vendor %q", target.Vendor)
	}
	fmt.Printf("provider=%s vendor=%s model=%s\n", target.ProviderID, target.Vendor, target.Model)

	events, err := adapter.Stream(ctx, provider.Request{
		Model: target.Model,
		Messages: []provider.Message{
			provider.TextMessage(provider.RoleUser, "Reply with exactly: nowhere-agent online"),
		},
		MaxTokens: 64,
	})
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}

	var text string
	for ev := range events {
		switch ev.Type {
		case provider.EventBlockDelta:
			text += ev.Delta
			fmt.Print(ev.Delta)
		case provider.EventError:
			return fmt.Errorf("stream error: %w", ev.Err)
		}
	}
	fmt.Println()
	if text == "" {
		return fmt.Errorf("no text received")
	}
	fmt.Println("--- OK ---")
	return nil
}
