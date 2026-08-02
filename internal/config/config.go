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
	// Permission configures the execution-permission gate over tool risks.
	Permission Permission
	// Dreaming configures the offline dreaming worker and the scheduler that
	// drives it (capability-gaps K1+K2).
	Dreaming Dreaming
	// Skills configures the skill runtime (capability-gap K3a).
	Skills Skills
	// Identity configures the account layer, notably platform-admin bootstrap
	// (admin-console).
	Identity Identity
}

// Permission maps each tool risk class to a decision for the execution-permission
// gate: allow (run), deny (block), or ask (headless server has no interactive
// approver, so ask is treated as deny). Defaults permit read-only, sandbox-write,
// and network (the wired web-search tool) and deny external-write; tighten
// network to deny for stricter multi-tenant isolation.
type Permission struct {
	ReadOnly      string `envconfig:"PERMISSION_READ_ONLY" default:"allow"`
	SandboxWrite  string `envconfig:"PERMISSION_SANDBOX_WRITE" default:"allow"`
	Network       string `envconfig:"PERMISSION_NETWORK" default:"allow"`
	ExternalWrite string `envconfig:"PERMISSION_EXTERNAL_WRITE" default:"deny"`
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

// Dreaming configures the offline dreaming worker (capability-gap K1) and the
// scheduler that drives it (K2). Disabled by default: each pass spends LLM
// tokens and requires a configured provider, so it is an explicit opt-in. When
// enabled, the server starts a scheduler that runs the worker every Interval,
// consolidating ended sessions' episodes into long-term memory, bounded by
// MaxTokens per pass.
type Dreaming struct {
	Enabled   bool          `envconfig:"DREAMING_ENABLED" default:"false"`
	Interval  time.Duration `envconfig:"DREAMING_INTERVAL" default:"1h"`
	MaxTokens int           `envconfig:"DREAMING_MAX_TOKENS" default:"100000"`

	// MaxFacts, MaxInsights and MaxSummaries cap the LIVE memories of each kind
	// in one scope. Consolidation is told each cap and its current count and
	// asked to merge to fit; anything still over is evicted oldest-first.
	//
	// Caps are per kind, not one shared total, because a shared total is won by
	// whichever kind generates most freely. That is not hypothetical: before this
	// existed, insights reached 83% of a live store whose facts were the part
	// with any value.
	//
	// Facts and preferences share MaxFacts — both are "things true about the
	// user", and splitting them would force an arbitrary line between "prefers
	// X" and "is X".
	MaxFacts     int `envconfig:"DREAMING_MAX_FACTS" default:"80"`
	MaxInsights  int `envconfig:"DREAMING_MAX_INSIGHTS" default:"30"`
	MaxSummaries int `envconfig:"DREAMING_MAX_SUMMARIES" default:"40"`

	// PurgeAfter is how long a deprecated memory is kept before permanent
	// deletion. Deprecation is reversible by design, so something has to close
	// the window; without it the store grows without bound in rows nothing can
	// recall.
	PurgeAfter time.Duration `envconfig:"DREAMING_PURGE_AFTER" default:"720h"`
}

// Validate rejects a dreaming configuration that cannot hold its invariants.
// A zero or negative cap is refused rather than read as "unbounded": the caps
// exist because an unbounded store is the failure being fixed, so silently
// restoring it via a typo'd env var would be the worst possible reading.
func (d Dreaming) Validate() error {
	if !d.Enabled {
		return nil
	}
	for _, c := range []struct {
		name string
		v    int
	}{
		{"DREAMING_MAX_FACTS", d.MaxFacts},
		{"DREAMING_MAX_INSIGHTS", d.MaxInsights},
		{"DREAMING_MAX_SUMMARIES", d.MaxSummaries},
	} {
		if c.v <= 0 {
			return fmt.Errorf("%s must be positive, got %d (there is no 'unbounded' setting)", c.name, c.v)
		}
	}
	if d.PurgeAfter <= 0 {
		return fmt.Errorf("DREAMING_PURGE_AFTER must be positive, got %s", d.PurgeAfter)
	}
	return nil
}

// Skills configures the skill runtime (capability-gap K3a). Dir points at a
// directory of skills (one subdirectory each, holding a SKILL.md); when set,
// the server seeds the skill store from it at startup at system scope, which
// lights up the L0 skill index in the system prompt and the read-only load_skill
// tool. Empty leaves the runtime dormant (no skills). Skill script execution is
// NOT enabled by this loader — that is K3b, gated on the C17 exec-safety fix.
type Skills struct {
	Dir string `envconfig:"SKILLS_DIR" default:""`
}

// Subagent configures the spawn_agent tool. It is only wired when a sandbox
// backend is configured (subagents need a tool pool). MaxDepth bounds recursive
// nesting; a child at the maximum depth does not receive the spawn tool.
type Subagent struct {
	Enabled  bool `envconfig:"SUBAGENT_ENABLED" default:"true"`
	MaxDepth int  `envconfig:"SUBAGENT_MAX_DEPTH" default:"3"`
	// MaxTotal caps the total subagent runs per top-level request; MaxConcurrent
	// caps how many run at once. Together they bound fan-out cost / fork bombs.
	MaxTotal      int `envconfig:"SUBAGENT_MAX_TOTAL" default:"32"`
	MaxConcurrent int `envconfig:"SUBAGENT_MAX_CONCURRENT" default:"8"`
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
	// LocalExec enables the run_command tool on the local backend. The local
	// backend runs commands on the host (confined only to the workspace working
	// dir), so this is a trusted single-tenant switch; multi-tenant deployments
	// should use the docker backend, where run_command is always available and
	// contained. Ignored by the docker (always on) and off backends.
	LocalExec bool `envconfig:"SANDBOX_LOCAL_EXEC" default:"true"`
	// Shell overrides the bash executable used by the local backend's
	// run_command. Empty auto-detects (bash on PATH; Git Bash on Windows). Set it
	// to your Git Bash bash.exe if auto-detection fails.
	Shell string `envconfig:"SANDBOX_SHELL" default:""`
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

// Identity configures the account layer (admin-console). The first account
// created on an empty platform is made a platform admin automatically, which
// does nothing for a deployment whose accounts predate the role — those
// designate one here.
type Identity struct {
	// BootstrapAdminEmail names an existing account to promote to platform
	// admin at startup. Applied idempotently on every boot; an email matching
	// no account logs a warning rather than failing startup, so a stale value
	// never blocks a deploy. It is also the recovery path if the last admin
	// loses the role.
	BootstrapAdminEmail string `envconfig:"BOOTSTRAP_ADMIN_EMAIL" default:""`
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
	if err := c.Dreaming.Validate(); err != nil {
		return Config{}, fmt.Errorf("dreaming config: %w", err)
	}
	return c, nil
}
