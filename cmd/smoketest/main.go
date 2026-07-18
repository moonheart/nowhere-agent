// Command smoketest calls the configured real LLM endpoint once with a trivial
// prompt and prints the streamed canonical events. It reads LLM_* env vars
// (source .env first). Used to verify the provider adapter against a live
// model — not part of the test suite.
package main

import (
	"context"
	"fmt"
	"os"

	"nowhere-agent/internal/config"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/provider/anthropic"
	"nowhere-agent/internal/provider/openai"
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
	if cfg.LLM.APIKey == "" {
		return fmt.Errorf("LLM_API_KEY not set (source .env)")
	}

	var adapter provider.Adapter
	switch cfg.LLM.Provider {
	case "anthropic":
		var opts []anthropic.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, anthropic.WithEndpoint(cfg.LLM.BaseURL))
		}
		adapter = anthropic.New(cfg.LLM.APIKey, opts...)
	case "openai":
		var opts []openai.Option
		if cfg.LLM.BaseURL != "" {
			opts = append(opts, openai.WithEndpoint(cfg.LLM.BaseURL))
		}
		adapter = openai.New(cfg.LLM.APIKey, opts...)
	default:
		return fmt.Errorf("LLM_PROVIDER must be anthropic|openai, got %q", cfg.LLM.Provider)
	}

	fmt.Printf("provider=%s model=%s\n", adapter.Name(), cfg.LLM.Model)

	events, err := adapter.Stream(context.Background(), provider.Request{
		Model: cfg.LLM.Model,
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
