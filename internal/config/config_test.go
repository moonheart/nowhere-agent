package config

import (
	"os"
	"path/filepath"
	"testing"
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
