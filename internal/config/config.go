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
	// Stream configures the live content broker (redis-stream-live).
	Stream Stream
	// Sandbox configures the per-session sandbox backend for built-in tools.
	Sandbox Sandbox
	// Subagent configures the spawn_agent tool (subagent capability).
	Subagent Subagent
	// MCP configures the MCP client integrations (mcp capability).
	MCP MCP
}

// MCP configures the built-in SearXNG MCP integration. Enabled is off by
// default; when on, the server connects to the SearXNG MCP endpoint over
// Streamable HTTP and registers its tools into each run's tool registry.
// SearxngURL defaults to the hosted instance and may be overridden for a
// self-hosted deployment.
type MCP struct {
	Enabled    bool   `envconfig:"MCP_ENABLED" default:"false"`
	SearxngURL string `envconfig:"MCP_SEARXNG_URL" default:"https://searxng-mcp.moonheart.dev/mcp"`
}

// Subagent configures the spawn_agent tool. It is only wired when a sandbox
// backend is configured (subagents need a tool pool). MaxDepth bounds recursive
// nesting; a child at the maximum depth does not receive the spawn tool.
type Subagent struct {
	Enabled  bool `envconfig:"SUBAGENT_ENABLED" default:"true"`
	MaxDepth int  `envconfig:"SUBAGENT_MAX_DEPTH" default:"3"`
}

// Sandbox selects the per-session sandbox backend that built-in file tools run
// against (file-tools). "off" (default) registers no tools; "local" confines
// files to a per-session host directory; "docker" isolates each session in a
// container.
type Sandbox struct {
	Backend string `envconfig:"SANDBOX_BACKEND" default:"off"` // off | local | docker
	// WorkspaceDir is the host root for the local backend's per-session
	// directories. Empty falls back to WORKSPACE_DIR.
	WorkspaceDir string `envconfig:"SANDBOX_WORKSPACE_DIR" default:""`
	// Network is the container egress policy for the docker backend: deny
	// (default, no egress), open (full egress), or allowlist (enforced by an
	// egress proxy; until that exists it fails closed to no egress). The local
	// backend ignores it. Default deny: the sandbox exists for isolation and the
	// wired tools (file I/O) need no network — multi-tenant docker deployments
	// should keep it deny (or allowlist) rather than open.
	Network string `envconfig:"SANDBOX_NETWORK" default:"deny"` // deny | open | allowlist
}

// Stream selects the live content broker that fans run output out to clients.
// "mem" (default) is the single-instance in-memory broker; "redis" fans out
// across instances via Redis Streams.
type Stream struct {
	Broker string `envconfig:"STREAM_BROKER" default:"mem"` // mem | redis
	// RedisAddr is the Redis address used when Broker=redis.
	RedisAddr string `envconfig:"REDIS_ADDR" default:"localhost:6379"`
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
	// ContextWindow is the model's context window in tokens. The agent loop
	// compresses its working view when it approaches this window (context-
	// compression). 0 disables in-loop compression.
	ContextWindow int `envconfig:"LLM_CONTEXT_WINDOW" default:"0"`
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
	DSN             string        `envconfig:"DB_DSN" default:"postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"`
	MaxOpenConns    int           `envconfig:"DB_MAX_OPEN_CONNS" default:"20"`
	MaxIdleConns    int           `envconfig:"DB_MAX_IDLE_CONNS" default:"5"`
	ConnMaxLifetime time.Duration `envconfig:"DB_CONN_MAX_LIFETIME" default:"30m"`
}

// Log configures logging.
type Log struct {
	Level  string `envconfig:"LOG_LEVEL" default:"debug"` // debug|info|warn|error
	Format string `envconfig:"LOG_FORMAT" default:"text"` // text|json
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
