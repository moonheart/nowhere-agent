package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// With no env overrides, defaults should apply.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr == "" {
		t.Error("expected default HTTP addr")
	}
	if cfg.DB.DSN == "" {
		t.Error("expected default DB DSN")
	}
	if cfg.Log.Level == "" {
		t.Error("expected default log level")
	}
}

func TestMCPDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MCP.Enabled {
		t.Error("MCP should be disabled by default")
	}
	if cfg.MCP.SearxngURL != "https://searxng-mcp.moonheart.dev/mcp" {
		t.Errorf("got searxng url %q, want hosted default", cfg.MCP.SearxngURL)
	}
}

func TestMCPFromEnv(t *testing.T) {
	t.Setenv("MCP_ENABLED", "true")
	t.Setenv("MCP_SEARXNG_URL", "http://localhost:9999/mcp")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MCP.Enabled {
		t.Error("MCP should be enabled from MCP_ENABLED=true")
	}
	if cfg.MCP.SearxngURL != "http://localhost:9999/mcp" {
		t.Errorf("got searxng url %q, want override", cfg.MCP.SearxngURL)
	}
}

func TestDreamingDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dreaming.Enabled {
		t.Error("dreaming should be disabled by default (it spends LLM tokens)")
	}
	if cfg.Dreaming.Interval != time.Hour {
		t.Errorf("got dreaming interval %v, want 1h", cfg.Dreaming.Interval)
	}
	if cfg.Dreaming.MaxTokens != 100000 {
		t.Errorf("got dreaming max tokens %d, want 100000", cfg.Dreaming.MaxTokens)
	}
	if cfg.Dreaming.MaxFacts != 80 || cfg.Dreaming.MaxInsights != 30 || cfg.Dreaming.MaxSummaries != 40 {
		t.Errorf("got caps facts=%d insights=%d summaries=%d, want 80/30/40",
			cfg.Dreaming.MaxFacts, cfg.Dreaming.MaxInsights, cfg.Dreaming.MaxSummaries)
	}
	if cfg.Dreaming.PurgeAfter != 720*time.Hour {
		t.Errorf("got purge-after %v, want 720h", cfg.Dreaming.PurgeAfter)
	}
}

func TestDreamingCapsFromEnv(t *testing.T) {
	t.Setenv("DREAMING_ENABLED", "true")
	t.Setenv("DREAMING_MAX_FACTS", "10")
	t.Setenv("DREAMING_MAX_INSIGHTS", "5")
	t.Setenv("DREAMING_MAX_SUMMARIES", "7")
	t.Setenv("DREAMING_PURGE_AFTER", "48h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dreaming.MaxFacts != 10 || cfg.Dreaming.MaxInsights != 5 || cfg.Dreaming.MaxSummaries != 7 {
		t.Errorf("got caps facts=%d insights=%d summaries=%d, want 10/5/7",
			cfg.Dreaming.MaxFacts, cfg.Dreaming.MaxInsights, cfg.Dreaming.MaxSummaries)
	}
	if cfg.Dreaming.PurgeAfter != 48*time.Hour {
		t.Errorf("got purge-after %v, want 48h", cfg.Dreaming.PurgeAfter)
	}
}

// A cap of zero must be refused, not read as "unbounded". An unbounded store is
// the failure these caps exist to fix, so restoring it via a typo'd env var
// would be the worst available reading of the value.
func TestDreamingNonPositiveCapRejected(t *testing.T) {
	for _, key := range []string{"DREAMING_MAX_FACTS", "DREAMING_MAX_INSIGHTS", "DREAMING_MAX_SUMMARIES"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("DREAMING_ENABLED", "true")
			t.Setenv(key, "0")
			if _, err := Load(); err == nil {
				t.Errorf("Load succeeded with %s=0; want an error", key)
			}
		})
	}
}

func TestDreamingNonPositivePurgeRejected(t *testing.T) {
	t.Setenv("DREAMING_ENABLED", "true")
	t.Setenv("DREAMING_PURGE_AFTER", "0s")
	if _, err := Load(); err == nil {
		t.Error("Load succeeded with DREAMING_PURGE_AFTER=0s; want an error")
	}
}

// Validation is scoped to an enabled worker: a deployment that never runs
// dreaming should not have to hold valid caps for it.
func TestDreamingCapsUnvalidatedWhenDisabled(t *testing.T) {
	t.Setenv("DREAMING_ENABLED", "false")
	t.Setenv("DREAMING_MAX_INSIGHTS", "0")
	if _, err := Load(); err != nil {
		t.Errorf("Load: %v; caps should not be validated while dreaming is off", err)
	}
}

func TestDreamingFromEnv(t *testing.T) {
	t.Setenv("DREAMING_ENABLED", "true")
	t.Setenv("DREAMING_INTERVAL", "5m")
	t.Setenv("DREAMING_MAX_TOKENS", "50000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Dreaming.Enabled {
		t.Error("dreaming should be enabled from DREAMING_ENABLED=true")
	}
	if cfg.Dreaming.Interval != 5*time.Minute {
		t.Errorf("got dreaming interval %v, want 5m", cfg.Dreaming.Interval)
	}
	if cfg.Dreaming.MaxTokens != 50000 {
		t.Errorf("got dreaming max tokens %d, want 50000", cfg.Dreaming.MaxTokens)
	}
}

func TestScheduleDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Schedule.Enabled {
		t.Error("schedule trigger should default to enabled")
	}
	if cfg.Schedule.ScanInterval != 30*time.Second {
		t.Errorf("got schedule scan interval %v, want 30s", cfg.Schedule.ScanInterval)
	}
}

func TestScheduleFromEnv(t *testing.T) {
	t.Setenv("SCHEDULE_ENABLED", "false")
	t.Setenv("SCHEDULE_SCAN_INTERVAL", "1m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Schedule.Enabled {
		t.Error("schedule trigger should be disabled from SCHEDULE_ENABLED=false")
	}
	if cfg.Schedule.ScanInterval != time.Minute {
		t.Errorf("got schedule scan interval %v, want 1m", cfg.Schedule.ScanInterval)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":9999" {
		t.Errorf("got addr %q want :9999", cfg.HTTP.Addr)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("got log level %q want warn", cfg.Log.Level)
	}
}

func TestLoadReadsDotEnv(t *testing.T) {
	// A .env in the working directory is loaded automatically.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LOG_LEVEL=error\nHTTP_ADDR=:7777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// godotenv.Load() reads .env from the CWD, so chdir into the temp dir.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("got log level %q want error (from .env)", cfg.Log.Level)
	}
	if cfg.HTTP.Addr != ":7777" {
		t.Errorf("got addr %q want :7777 (from .env)", cfg.HTTP.Addr)
	}
}

func TestDotEnvDoesNotOverrideRealEnv(t *testing.T) {
	// A real environment variable beats the .env file value.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LOG_LEVEL=error\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_LEVEL", "warn") // real env wins over .env's "error"

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("real env should win over .env: got %q want warn", cfg.Log.Level)
	}
}
