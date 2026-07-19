// Package config loads application configuration from environment variables
// and optional files. It is the single source of runtime configuration.
package config

import (
	"fmt"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

// Config is the root application configuration.
type Config struct {
	Env  string `envconfig:"ENV" default:"dev"` // dev | staging | prod
	HTTP HTTP
	DB   DB
	Log  Log
	LLM  LLM
	Web  Web
	// Workspace configures per-session image storage (persist-raw-messages).
	Workspace Workspace
}

// LLM configures the default model provider for the chat endpoint.
type LLM struct {
	Provider string `envconfig:"LLM_PROVIDER" default:""` // anthropic | openai
	Model    string `envconfig:"LLM_MODEL" default:""`
	APIKey   string `envconfig:"LLM_API_KEY" default:""`
	BaseURL  string `envconfig:"LLM_BASE_URL" default:""` // optional override/proxy
	// RawLogDir, when set, records every raw LLM HTTP request/response pair to
	// <dir>/<provider>/<timestamp>-<seq>.{req,resp} for offline inspection.
	// Auth headers are never written. Empty disables recording.
	RawLogDir string `envconfig:"LLM_RAW_LOG_DIR" default:""`
}

// Web configures serving the built frontend.
type Web struct {
	// Dir is the built frontend (web/dist). Empty disables static serving.
	Dir string `envconfig:"WEB_DIR" default:""`
}

// Workspace configures the per-session workspace storage that backs image
// payloads referenced by conversation messages (persist-raw-messages).
type Workspace struct {
	// Dir is the local root under which per-session image files are stored
	// (<dir>/<sessionID>/<name>.webp). Empty disables image storage.
	Dir string `envconfig:"WORKSPACE_DIR" default:""`
}

// HTTP configures the gateway server.
type HTTP struct {
	Addr            string        `envconfig:"HTTP_ADDR" default:":8080"`
	ReadTimeout     time.Duration `envconfig:"HTTP_READ_TIMEOUT" default:"30s"`
	WriteTimeout    time.Duration `envconfig:"HTTP_WRITE_TIMEOUT" default:"60s"`
	ShutdownTimeout time.Duration `envconfig:"HTTP_SHUTDOWN_TIMEOUT" default:"15s"`
}

// DB configures Postgres.
type DB struct {
	DSN             string `envconfig:"DB_DSN" default:"postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"`
	MaxOpenConns    int    `envconfig:"DB_MAX_OPEN_CONNS" default:"20"`
	MaxIdleConns    int    `envconfig:"DB_MAX_IDLE_CONNS" default:"5"`
	ConnMaxLifetime time.Duration `envconfig:"DB_CONN_MAX_LIFETIME" default:"30m"`
}

// Log configures logging.
type Log struct {
	Level  string `envconfig:"LOG_LEVEL" default:"debug"` // debug|info|warn|error
	Format string `envconfig:"LOG_FORMAT" default:"text"`  // text|json
}

// Load reads configuration from the environment. It first loads a local .env
// file (if present) without overriding variables already set in the process
// environment — so real env vars (e.g. injected in prod) always win.
func Load() (Config, error) {
	// Best-effort: missing .env is fine (prod injects real env vars).
	_ = godotenv.Load()

	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return Config{}, fmt.Errorf("process env config: %w", err)
	}
	return c, nil
}
