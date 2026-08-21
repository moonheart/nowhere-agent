// Package settings provides runtime-settable platform configuration: keys the
// operator changes from the admin console instead of by editing env and
// restarting. Boot semantics: env values remain the defaults; a row in
// platform_settings overrides them for the life of the process (or until the
// console writes a new value).
//
// The Runtime keeps an in-memory snapshot loaded from Postgres at boot.
// Reads are lock-free after load (RWMutex); a write goes to Postgres first
// and then updates the snapshot, so the change applies on the next use with
// no restart. Multi-instance deployments converge on the next write from any
// console; a boot reload picks up rows written elsewhere.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// ErrNotFound is returned when no row exists for a key.
var ErrNotFound = errors.New("settings: key not found")

// Store persists setting rows in Postgres.
type Store struct {
	db *sql.DB
}

// NewStore builds a Store over a database handle.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Get loads one row's raw JSON value.
func (s *Store) Get(ctx context.Context, key string) (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM platform_settings WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// List loads every row into a map.
func (s *Store) List(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM platform_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var raw []byte
		if err := rows.Scan(&k, &raw); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(raw)
	}
	return out, rows.Err()
}

// Set upserts one row. nil/JSON-null removes the row (back to the env default).
func (s *Store) Set(ctx context.Context, key string, value json.RawMessage) error {
	if len(value) == 0 || string(value) == "null" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM platform_settings WHERE key = $1`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO platform_settings (key, value, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, []byte(value))
	return err
}

// Runtime is the in-memory settings view every read path consults.
type Runtime struct {
	store *Store
	log   *slog.Logger

	mu     sync.RWMutex
	values map[string]json.RawMessage
	// defaults holds the boot-time env-derived values, used only when no row
	// overrides them.
	defaults map[string]json.RawMessage
}

// NewRuntime builds a Runtime seeded with defaults (env values). Call Load
// before serving to pick up persisted rows.
func NewRuntime(store *Store, defaults map[string]json.RawMessage, log *slog.Logger) *Runtime {
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{
		store: store, log: log,
		values:   map[string]json.RawMessage{},
		defaults: defaults,
	}
}

// Load refreshes the snapshot from the store (called at boot; also usable as
// a manual reload).
func (rt *Runtime) Load(ctx context.Context) error {
	rows, err := rt.store.List(ctx)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.values = rows
	rt.mu.Unlock()
	return nil
}

// StartRefreshLoop re-loads the snapshot from the store on a fixed interval
// until ctx is cancelled. It is the multi-instance convergence path (P2-6):
// rows written by another gateway process — or directly in the database —
// reach this process's snapshot within one interval, without a restart or a
// local console write. Load errors are logged and skipped, so a DB hiccup
// degrades to a stale snapshot instead of killing the loop. interval <= 0
// falls back to the default 30s. Returns immediately; the goroutine stops on
// ctx.Done.
func (rt *Runtime) StartRefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Recover per tick so a panic in Load cannot kill the refresh
				// loop: multi-instance convergence must survive one bad read.
				func() {
					defer func() {
						if p := recover(); p != nil {
							rt.log.Error("platform settings refresh panicked", "panic", p, "stack", string(debug.Stack()))
						}
					}()
					if err := rt.Load(ctx); err != nil {
						rt.log.Warn("platform settings refresh failed; keeping the previous snapshot", "err", err)
					}
				}()
			}
		}
	}()
}

// raw returns the effective value for key: the persisted row, else the
// default. Second return is false when neither exists.
func (rt *Runtime) raw(key string) (json.RawMessage, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if v, ok := rt.values[key]; ok {
		return v, true
	}
	v, ok := rt.defaults[key]
	return v, ok
}

// String returns the effective string value for key ("" when unset).
func (rt *Runtime) String(key string) string {
	raw, ok := rt.raw(key)
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Int returns the effective integer value (0 when unset or malformed).
func (rt *Runtime) Int(key string) int {
	raw, ok := rt.raw(key)
	if !ok {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

// Bool returns the effective boolean value (false when unset or malformed).
func (rt *Runtime) Bool(key string) bool {
	raw, ok := rt.raw(key)
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false
	}
	return b
}

// Float64 returns the effective float value (0 when unset or malformed).
func (rt *Runtime) Float64(key string) float64 {
	raw, ok := rt.raw(key)
	if !ok {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0
	}
	return f
}

// Duration returns the effective duration for an integer-seconds key
// (0 when unset or malformed). A stored 0 means "0s", not "default" — the
// callers define what zero disables.
func (rt *Runtime) Duration(key string) time.Duration {
	return time.Duration(rt.Int(key)) * time.Second
}

// Set writes a value (Postgres first, then the local snapshot). A JSON null
// removes the row, returning the key to its env default.
func (rt *Runtime) Set(ctx context.Context, key string, value json.RawMessage) error {
	if err := rt.store.Set(ctx, key, value); err != nil {
		return err
	}
	rt.mu.Lock()
	if len(value) == 0 || string(value) == "null" {
		delete(rt.values, key)
	} else {
		rt.values[key] = value
	}
	rt.mu.Unlock()
	rt.log.Info("platform setting updated", "key", key)
	return nil
}

// Kind is the value type of a setting, driving the admin console's input
// widget and PUT validation.
type Kind string

// Supported value kinds.
const (
	KindString Kind = "string"
	KindInt    Kind = "int"
	KindFloat  Kind = "float"
	KindBool   Kind = "bool"
)

// Group names the admin-console tab a setting lives under.
type Group string

// Admin-console tabs.
const (
	GroupTools        Group = "tools"
	GroupWebhooks     Group = "webhooks"
	GroupLLM          Group = "llm"
	GroupSandbox      Group = "sandbox"
	GroupPermissions  Group = "permissions"
	GroupRedaction    Group = "redaction"
	GroupSubagents    Group = "subagents"
	GroupBackground   Group = "background"
	GroupHTTP         Group = "http"
	GroupAuth         Group = "auth"
	GroupIntegrations Group = "integrations"
)

// KeyInfo describes one known setting for the admin surface.
type KeyInfo struct {
	Key   string
	Group Group
	Kind  Kind
	// Secret marks values that must never be echoed back (shown as
	// "set/unset" in the console; e.g. the webhook signing secret).
	Secret      bool
	Description string
}

// Catalog lists every known setting in console display order, with its tab,
// value kind, secrecy, and help text.
func Catalog() []KeyInfo {
	return []KeyInfo{
		// Tools tab.
		{Key: KeyHTTPToolAllowlist, Group: GroupTools, Kind: KindString, Description: "Comma-separated http_request host allowlist (same syntax as HTTP_TOOL_ALLOWLIST): api.example.com, *.example.com, 10.0.0.0/8, or *. Empty disables the tool. Applies to the next run."},
		{Key: KeyHTTPToolTimeout, Group: GroupTools, Kind: KindInt, Description: "http_request per-call timeout in seconds (overrides HTTP_TOOL_TIMEOUT)."},
		{Key: KeyHTTPToolMaxConcurrent, Group: GroupTools, Kind: KindInt, Description: "Max tool executions one run may have in flight at once, across the whole tool registry (overrides HTTP_TOOL_MAX_CONCURRENT). 0 = unlimited. Applies to the next run."},
		{Key: KeyQueryDBDsns, Group: GroupTools, Kind: KindString, Secret: true, Description: "Comma-separated name=dsn business databases for query_db (same syntax as QUERY_DB_DSNS), e.g. erp=postgres://ro:secret@pg.internal:5432/erp. Empty disables the tool. DSNs may carry database passwords — the value is never echoed back. Applies to the next run."},
		{Key: KeyQueryDBTimeout, Group: GroupTools, Kind: KindInt, Description: "query_db per-call timeout in seconds (overrides QUERY_DB_TIMEOUT)."},
		{Key: KeyRunCommandTimeout, Group: GroupTools, Kind: KindInt, Description: "run_command per-call timeout in seconds (overrides RUN_COMMAND_TIMEOUT). Applies to the next run."},

		// Webhooks tab.
		{Key: KeyWebhookURL, Group: GroupWebhooks, Kind: KindString, Description: "Global run-completion notification target (overrides WEBHOOK_URL; task-level and inbound-webhook URLs still win per run). Applies to the next notification."},
		{Key: KeyWebhookTimeout, Group: GroupWebhooks, Kind: KindInt, Description: "Timeout of one webhook delivery attempt in seconds (overrides WEBHOOK_TIMEOUT)."},
		{Key: KeyWebhookRetries, Group: GroupWebhooks, Kind: KindInt, Description: "Delivery attempts after the first, with exponential backoff (overrides WEBHOOK_RETRIES). 4xx responses are final."},
		{Key: KeyWebhookSSRFAllowlist, Group: GroupWebhooks, Kind: KindString, Description: "Comma-separated escape hatch for internal notification targets: CIDR blocks (10.0.0.0/8) and/or exact hostnames (im.example.internal). Empty = strict: only public targets. A malformed entry disables only the dynamic override (the boot allowlist stays)."},
		{Key: KeyWebhookSigningSecret, Group: GroupWebhooks, Kind: KindString, Secret: true, Description: "HMAC-SHA256 signing secret for notification payloads (X-Nowhere-Signature; same role as WEBHOOK_SIGNING_SECRET). Empty = unsigned. The value is never echoed back."},

		// LLM tab.
		{Key: KeySystemLang, Group: GroupLLM, Kind: KindString, Description: "System-prompt language for new runs: en | zh (overrides LLM_SYSTEM_LANG)."},
		{Key: KeyLLMContextWindow, Group: GroupLLM, Kind: KindInt, Description: "Model context window in tokens for in-loop compression (overrides LLM_CONTEXT_WINDOW). 0 = derive from the model's capability profile."},
		{Key: KeyLLMTemperature, Group: GroupLLM, Kind: KindFloat, Description: "Sampling temperature for chat runs (overrides LLM_TEMPERATURE). Negative = provider default."},
		{Key: KeyLLMThinkingBudget, Group: GroupLLM, Kind: KindInt, Description: "Extended-reasoning token budget (overrides LLM_THINKING_BUDGET). 0 disables."},
		{Key: KeyAgentMaxIterations, Group: GroupLLM, Kind: KindInt, Description: "Loop iteration cap for chat runs (overrides LLM_MAX_ITERATIONS). 0 = built-in default (25). Applies to the next run."},
		{Key: KeyLLMStreamIdleTimeout, Group: GroupLLM, Kind: KindInt, Description: "Stream stall guard in seconds: fail a generation that sends no bytes for this long (overrides LLM_STREAM_IDLE_TIMEOUT). 0 disables the guard."},
		{Key: KeyLLMRawLogDir, Group: GroupLLM, Kind: KindString, Description: "Directory recording raw LLM request/response pairs for inspection (overrides LLM_RAW_LOG_DIR; auth headers never recorded). Empty disables. Applies from the next run."},
		{Key: KeyLLMRawLogRetentionDays, Group: GroupLLM, Kind: KindInt, Description: "Days raw LLM request/response logs are kept before the hourly sweep deletes them (overrides LLM_RAW_LOG_RETENTION_DAYS). 0 disables the sweep. Applies within an hour."},

		// Sandbox tab.
		{Key: KeySandboxNetwork, Group: GroupSandbox, Kind: KindString, Description: "Container egress policy: deny | open (overrides SANDBOX_NETWORK). allowlist is accepted for compatibility but NOT implemented — the docker backend maps it to deny (zero egress); the local backend cannot restrict host egress at all and logs a loud warning each session when the policy is not open. Applies to the next session."},
		{Key: KeySandboxLocalExec, Group: GroupSandbox, Kind: KindBool, Description: "Enable the run_command tool on the local (host) sandbox backend (overrides SANDBOX_LOCAL_EXEC). Trusted single-tenant switch; multi-tenant deployments should use docker. Applies to the next session."},

		// Permissions tab.
		{Key: KeyPermissionReadOnly, Group: GroupPermissions, Kind: KindString, Description: "Read-only-risk tools (query_db, recall_memory, …): allow | ask | deny (overrides PERMISSION_READ_ONLY)."},
		{Key: KeyPermissionSandboxWrite, Group: GroupPermissions, Kind: KindString, Description: "Sandbox-contained write tools (file tools, plan_write, …): allow | ask | deny (overrides PERMISSION_SANDBOX_WRITE)."},
		{Key: KeyPermissionNetwork, Group: GroupPermissions, Kind: KindString, Description: "Network tools (http_request, MCP tools, web search): allow | ask | deny (overrides PERMISSION_NETWORK)."},
		{Key: KeyPermissionExternalWrite, Group: GroupPermissions, Kind: KindString, Description: "External-write tools (long-term memory writes): allow | ask | deny (overrides PERMISSION_EXTERNAL_WRITE). 'ask' suspends for human approval."},

		// Redaction tab.
		{Key: KeyRedactEnabled, Group: GroupRedaction, Kind: KindBool, Description: "Redact PII/secrets from tool results before they reach the model or the durable record (overrides REDACT_ENABLED). Applies from the next run."},
		{Key: KeyRedactStrategy, Group: GroupRedaction, Kind: KindString, Description: "Redaction replacement: redact (whole value → [REDACTED_<TYPE>]) | mask (→ ***<last 4 chars>) (overrides REDACT_STRATEGY)."},
		{Key: KeyRedactCategories, Group: GroupRedaction, Kind: KindString, Description: "Comma-separated categories to redact: email, credit_card, ipv4, bearer, basic_auth, api_key, private_key, secret_value. Empty redacts all (overrides REDACT_CATEGORIES)."},

		// Subagents tab.
		{Key: KeySubagentEnabled, Group: GroupSubagents, Kind: KindBool, Description: "Register the spawn_agent tool into new runs (overrides SUBAGENT_ENABLED). Applies to the next run."},
		{Key: KeySubagentMaxDepth, Group: GroupSubagents, Kind: KindInt, Description: "Maximum subagent nesting depth; children at the maximum do not get the spawn tool (overrides SUBAGENT_MAX_DEPTH)."},
		{Key: KeySubagentMaxTotal, Group: GroupSubagents, Kind: KindInt, Description: "Total subagent runs per top-level request (overrides SUBAGENT_MAX_TOTAL)."},
		{Key: KeySubagentMaxConcurrent, Group: GroupSubagents, Kind: KindInt, Description: "Subagent runs that may run at once (overrides SUBAGENT_MAX_CONCURRENT)."},

		// Background tab.
		{Key: KeyDreamingEnabled, Group: GroupBackground, Kind: KindBool, Description: "Periodically consolidate ended sessions into long-term memory (overrides DREAMING_ENABLED). Off keeps the console's manual trigger."},
		{Key: KeyDreamingInterval, Group: GroupBackground, Kind: KindInt, Description: "Dreaming pass interval in seconds (overrides DREAMING_INTERVAL). Takes effect at the next tick."},
		{Key: KeyDreamingMaxTokens, Group: GroupBackground, Kind: KindInt, Description: "LLM tokens a single dreaming pass may spend (overrides DREAMING_MAX_TOKENS)."},
		{Key: KeyDreamingMaxFacts, Group: GroupBackground, Kind: KindInt, Description: "Live long-term-memory cap for facts + preferences (overrides DREAMING_MAX_FACTS)."},
		{Key: KeyDreamingMaxInsights, Group: GroupBackground, Kind: KindInt, Description: "Live long-term-memory cap for insights (overrides DREAMING_MAX_INSIGHTS)."},
		{Key: KeyDreamingMaxSummaries, Group: GroupBackground, Kind: KindInt, Description: "Live long-term-memory cap for summaries (overrides DREAMING_MAX_SUMMARIES)."},
		{Key: KeyDreamingPurgeAfter, Group: GroupBackground, Kind: KindInt, Description: "Days a deprecated memory is kept before permanent deletion (overrides DREAMING_PURGE_AFTER)."},
		{Key: KeyScheduleEnabled, Group: GroupBackground, Kind: KindBool, Description: "Run due scheduled tasks automatically (overrides SCHEDULE_ENABLED). Off keeps task CRUD and run-now."},
		{Key: KeyScheduleScanInterval, Group: GroupBackground, Kind: KindInt, Description: "How often the scheduled-task trigger scans for due tasks, in seconds (overrides SCHEDULE_SCAN_INTERVAL). Takes effect within a few seconds."},

		// HTTP layer (gateway).
		{Key: KeyRateLimitRPS, Group: GroupHTTP, Kind: KindFloat, Description: "Per-IP HTTP rate limit, requests per second (overrides HTTP_RATE_LIMIT_RPS). 0 = disabled. Retuned live within a few seconds."},
		{Key: KeyRateLimitBurst, Group: GroupHTTP, Kind: KindInt, Description: "Per-IP HTTP rate limit burst size (overrides HTTP_RATE_LIMIT_BURST). 0 = disabled. Retuned live within a few seconds."},
		{Key: KeyUploadMaxFilesPerUser, Group: GroupHTTP, Kind: KindInt, Description: "Max image uploads one user may hold (overrides UPLOAD_MAX_FILES_PER_USER). 0 = unlimited. Applied per USER to user-level uploads (upload tab / first message) and per SESSION to a chat session's own images. Applied to the next upload."},
		{Key: KeyUploadMaxBytesPerUser, Group: GroupHTTP, Kind: KindInt, Description: "Max total stored upload bytes per user in bytes (overrides UPLOAD_MAX_BYTES_PER_USER). 0 = unlimited. Applied per USER to user-level uploads and per SESSION to a chat session's own images. Applied to the next upload."},
		{Key: KeyWorkspaceRetentionDays, Group: GroupBackground, Kind: KindInt, Description: "Days an ENDED session's image directory is kept before the hourly sweep deletes it (overrides WORKSPACE_RETENTION_DAYS). 0 disables the sweep. Applies within an hour."},
		{Key: KeyConversationRetentionDays, Group: GroupBackground, Kind: KindInt, Description: "Days an ENDED session's conversation is kept before the hourly sweep hard-deletes it and its messages (overrides CONVERSATION_RETENTION_DAYS). 0 disables the sweep. Applies within an hour."},
		{Key: KeyAuditRetentionDays, Group: GroupBackground, Kind: KindInt, Description: "Days an audit-trail row is kept before the hourly sweep purges it (overrides AUDIT_RETENTION_DAYS). 0 disables the sweep — the trail is retained forever. Applies within an hour."},

		// Auth / SSO.
		{Key: KeyPhoneSMSURL, Group: GroupAuth, Kind: KindString, Description: "SMS-OTP gateway for phone login (overrides PHONE_SMS_URL): an http(s) URL that receives {\"phone\",\"code\"}, or log:// to print codes to the server log (dev only). Empty disables phone login on the login page. Applies to the next code request."},
		{Key: KeyPhoneSMSTimeout, Group: GroupAuth, Kind: KindInt, Description: "Timeout of one SMS gateway call in seconds (overrides PHONE_SMS_TIMEOUT)."},

		// Integrations.
		{Key: KeyMCPServers, Group: GroupIntegrations, Kind: KindString, Secret: true, Description: "MCP servers as a JSON array of {\"name\",\"url\",\"headers\",\"timeout\"} (overrides MCP_SERVERS). Add/remove/retune servers live; unchanged servers keep their session. Headers may carry bearer tokens — the value is never echoed back. Empty disables MCP tools."},
	}
}

// Keys returns the known runtime-setting keys (for validation and the admin
// surface).
func Keys() []string {
	infos := Catalog()
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Key)
	}
	return out
}

// Info returns the catalog entry for key (zero value when unknown).
func Info(key string) KeyInfo {
	for _, i := range Catalog() {
		if i.Key == key {
			return i
		}
	}
	return KeyInfo{}
}

// Keys is the runtime's view of the known keys (the admin surface lists
// them with their current effective values).
func (rt *Runtime) Keys() []string { return Keys() }

// Key names (documented in the admin console page).
const (
	// Tools tab.
	// KeyHTTPToolAllowlist is the comma-separated http_request host allowlist
	// (same syntax as HTTP_TOOL_ALLOWLIST).
	KeyHTTPToolAllowlist = "http_tool_allowlist"
	// KeyHTTPToolTimeout is the http_request per-call timeout in seconds
	// (overrides HTTP_TOOL_TIMEOUT).
	KeyHTTPToolTimeout = "http_tool_timeout"
	// KeyHTTPToolMaxConcurrent caps a run's in-flight tool executions across
	// the whole tool registry (overrides HTTP_TOOL_MAX_CONCURRENT); 0 =
	// unlimited.
	KeyHTTPToolMaxConcurrent = "http_tool_max_concurrent"
	// KeyQueryDBDsns is the comma-separated name=dsn list for query_db (same
	// syntax as QUERY_DB_DSNS).
	KeyQueryDBDsns = "query_db_dsns"
	// KeyQueryDBTimeout is the query_db per-call timeout in seconds
	// (overrides QUERY_DB_TIMEOUT).
	KeyQueryDBTimeout = "query_db_timeout"
	// KeyRunCommandTimeout is the run_command per-call timeout in seconds
	// (the tool's ceiling was previously a hardcoded 120s).
	KeyRunCommandTimeout = "run_command_timeout"

	// Webhooks tab.
	// KeyWebhookURL is the global run-completion notification target
	// (overrides WEBHOOK_URL; task/notify_url targets still win per run).
	KeyWebhookURL = "webhook_url"
	// KeyWebhookTimeout is one delivery attempt's timeout in seconds
	// (overrides WEBHOOK_TIMEOUT).
	KeyWebhookTimeout = "webhook_timeout"
	// KeyWebhookRetries is the delivery attempts after the first
	// (overrides WEBHOOK_RETRIES).
	KeyWebhookRetries = "webhook_retries"
	// KeyWebhookSSRFAllowlist is the comma-separated CIDR/host escape hatch
	// for internal notification targets (overrides WEBHOOK_SSRF_ALLOWLIST).
	KeyWebhookSSRFAllowlist = "webhook_ssrf_allowlist"
	// KeyWebhookSigningSecret is the HMAC-SHA256 payload signing secret
	// (overrides WEBHOOK_SIGNING_SECRET). Never echoed back by the console.
	KeyWebhookSigningSecret = "webhook_signing_secret"

	// LLM tab.
	// KeySystemLang is the system-prompt language ("en" | "zh"; overrides
	// LLM_SYSTEM_LANG for NEW runs).
	KeySystemLang = "llm_system_lang"
	// KeyLLMContextWindow is the model context window in tokens
	// (overrides LLM_CONTEXT_WINDOW).
	KeyLLMContextWindow = "llm_context_window"
	// KeyLLMTemperature is the sampling temperature (overrides
	// LLM_TEMPERATURE); negative = provider default.
	KeyLLMTemperature = "llm_temperature"
	// KeyLLMThinkingBudget is the extended-reasoning token budget
	// (overrides LLM_THINKING_BUDGET); 0 disables.
	KeyLLMThinkingBudget = "llm_thinking_budget"
	// KeyAgentMaxIterations is the chat run's loop iteration cap
	// (overrides LLM_MAX_ITERATIONS); 0 falls back to the built-in 25.
	KeyAgentMaxIterations = "agent_max_iterations"
	// KeyLLMStreamIdleTimeout is the stream stall guard in seconds
	// (overrides LLM_STREAM_IDLE_TIMEOUT); 0 disables.
	KeyLLMStreamIdleTimeout = "llm_stream_idle_timeout"
	// KeyLLMRawLogDir is the raw LLM wire-traffic recording directory
	// (overrides LLM_RAW_LOG_DIR); empty disables.
	KeyLLMRawLogDir = "llm_raw_log_dir"
	// KeyLLMRawLogRetentionDays is how long raw LLM wire-traffic logs are
	// kept in days before the hourly sweep deletes them (overrides
	// LLM_RAW_LOG_RETENTION_DAYS); 0 disables the sweep.
	KeyLLMRawLogRetentionDays = "llm_raw_log_retention_days"

	// Sandbox tab.
	// KeySandboxNetwork is the docker-backend container egress policy
	// (overrides SANDBOX_NETWORK): deny | open | allowlist.
	KeySandboxNetwork = "sandbox_network"
	// KeySandboxLocalExec enables run_command on the local backend
	// (overrides SANDBOX_LOCAL_EXEC).
	KeySandboxLocalExec = "sandbox_local_exec"

	// Permissions tab.
	KeyPermissionReadOnly      = "permission_read_only"
	KeyPermissionSandboxWrite  = "permission_sandbox_write"
	KeyPermissionNetwork       = "permission_network"
	KeyPermissionExternalWrite = "permission_external_write"

	// Redaction tab.
	KeyRedactEnabled    = "redact_enabled"
	KeyRedactStrategy   = "redact_strategy"
	KeyRedactCategories = "redact_categories"

	// Subagents tab.
	KeySubagentEnabled       = "subagent_enabled"
	KeySubagentMaxDepth      = "subagent_max_depth"
	KeySubagentMaxTotal      = "subagent_max_total"
	KeySubagentMaxConcurrent = "subagent_max_concurrent"

	// Background tab.
	KeyDreamingEnabled      = "dreaming_enabled"
	KeyDreamingInterval     = "dreaming_interval"
	KeyDreamingMaxTokens    = "dreaming_max_tokens"
	KeyDreamingMaxFacts     = "dreaming_max_facts"
	KeyDreamingMaxInsights  = "dreaming_max_insights"
	KeyDreamingMaxSummaries = "dreaming_max_summaries"
	KeyDreamingPurgeAfter   = "dreaming_purge_after"
	KeyScheduleEnabled      = "schedule_enabled"
	KeyScheduleScanInterval = "schedule_scan_interval"

	// Rate limits (HTTP layer).
	// KeyRateLimitRPS / KeyRateLimitBurst tune the per-IP HTTP limiter (0/0 =
	// disabled; overrides HTTP_RATE_LIMIT_*).
	KeyRateLimitRPS   = "rate_limit_rps"
	KeyRateLimitBurst = "rate_limit_burst"
	// KeyUploadMaxFilesPerUser caps one user's upload records (<= 0 = no cap;
	// overrides UPLOAD_MAX_FILES_PER_USER). Applied to the next upload.
	KeyUploadMaxFilesPerUser = "upload_max_files_per_user"
	// KeyUploadMaxBytesPerUser caps one user's total stored upload bytes
	// (<= 0 = no cap; overrides UPLOAD_MAX_BYTES_PER_USER). Applied to the
	// next upload.
	KeyUploadMaxBytesPerUser = "upload_max_bytes_per_user"
	// KeyWorkspaceRetentionDays is how long an ENDED session's image dir is
	// kept before the hourly sweep deletes it (overrides
	// WORKSPACE_RETENTION_DAYS); <= 0 disables the sweep.
	KeyWorkspaceRetentionDays = "workspace_retention_days"
	// KeyConversationRetentionDays is how long an ENDED session's conversation
	// (session row + cascaded runs/messages/events/approvals) is kept before
	// the hourly sweep hard-deletes it (overrides CONVERSATION_RETENTION_DAYS);
	// <= 0 disables the sweep.
	KeyConversationRetentionDays = "conversation_retention_days"
	// KeyAuditRetentionDays is how long an audit_log row is kept before the
	// hourly sweep purges it (overrides AUDIT_RETENTION_DAYS); <= 0 disables
	// the sweep.
	KeyAuditRetentionDays = "audit_retention_days"

	// Auth / SSO.
	// KeyPhoneSMSURL is the SMS-OTP gateway for phone login (overrides
	// PHONE_SMS_URL): http(s) URL or "log://" (dev); empty disables phone
	// login. Applied to the next code request.
	KeyPhoneSMSURL = "phone_sms_url"
	// KeyPhoneSMSTimeout bounds one SMS gateway call in seconds (overrides
	// PHONE_SMS_TIMEOUT).
	KeyPhoneSMSTimeout = "phone_sms_timeout"

	// Integrations.
	// KeyMCPServers is the MCP server list as MCP_SERVERS JSON (overrides
	// MCP_SERVERS; legacy MCP_ENABLED/MCP_SEARXNG_URL map to a single
	// "searxng" server). Secret: headers may carry bearer tokens.
	KeyMCPServers = "mcp_servers"
)
