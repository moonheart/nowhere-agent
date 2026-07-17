package config

import "testing"

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
