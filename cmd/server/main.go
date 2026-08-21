// Command server runs the nowhere-agent gateway.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/config"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/logging"
	"nowhere-agent/internal/mcp"
	"nowhere-agent/internal/observability"
	"nowhere-agent/internal/openapi"
	"nowhere-agent/internal/permission"
	"nowhere-agent/internal/platform/db"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/secrets"
	"nowhere-agent/internal/settings"
	"nowhere-agent/internal/trustedproxy"
	"nowhere-agent/internal/webhook"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// hourlySweep runs fn every hour on its own goroutine until ctx ends — the
// shared skeleton of the credential / raw-log / pending-interaction / image
// retention / inbound-nonce sweepers, which are all the same ticker+select
// with different work. The first pass runs immediately on startup (the
// ticker's first tick only fires after a full hour, so a just-booted instance
// would otherwise keep dead rows and stale image dirs around one hour longer
// than configured); each later pass is logged under name ("<name> sweep
// failed") and retried next tick; a slow pass simply delays the next tick
// (the ticker drops missed ticks). fn must be best-effort and cheap; it
// logs its own success detail (per-sweep wording and counts). fn may use ctx,
// which is alive for the startup pass.
func hourlySweep(ctx context.Context, log *slog.Logger, name string, fn func() error) {
	go func() {
		runSweep := func() {
			if err := fn(); err != nil {
				log.Warn(name+" sweep failed", "err", err)
			}
		}
		runSweep()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runSweep()
			}
		}
	}()
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(log)

	// Client-IP trust boundary (P0-1): proxy headers (X-Forwarded-For,
	// X-Real-IP) are honoured only for peers in HTTP_TRUSTED_PROXY_CIDRS. The
	// empty default trusts nothing, so a spoofable header cannot forge the IP
	// the rate limiter and audit trail key on. This is a global process
	// setting because the audit path resolves IPs at event-build time, far from
	// any handler that could carry the set.
	trustedproxy.SetDefault(cfg.HTTP.TrustedProxyCIDRs)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DB.DSN, cfg.DB.MaxOpenConns, cfg.DB.MaxIdleConns, cfg.DB.ConnMaxLifetime)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("connected to database")

	// Health probe (enterprise-readiness P0-3): /healthz reports liveness as the
	// AND of registered dependency probes, so an orchestrator can tell "process
	// alive but database dead" apart from healthy. Probes are added as each
	// dependency comes up below; Postgres is the first.
	health := observability.NewHealthz(0)
	health.Add("postgres", func(context.Context) error { return pool.Ping() })

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler())

	// Metrics (enterprise-readiness P0-3): per-route request counts and latency,
	// served at /metrics for Prometheus/VictoriaMetrics to scrape. The route
	// label is the ServeMux pattern (r.Pattern), not the raw path, so
	// /api/users/{id} stays one series regardless of how many ids exist.
	metrics := observability.NewMetrics()
	mux.Handle("GET /metrics", metrics.Handler())

	// OpenAPI contract (enterprise integration): the embeddable API surface as
	// a machine-readable document, so external systems generate typed clients
	// instead of reverse-engineering the routes. Open (no auth): it describes
	// the API, it does not expose it.
	if spec, err := openapi.JSON(); err != nil {
		return fmt.Errorf("openapi spec: %w", err)
	} else {
		mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(spec)
		})
	}
	log.Info("openapi contract served at /openapi.json")

	// Runtime-settable platform settings (no-restart configuration): operator
	// knobs that used to be env-only now default from env at boot and can be
	// overridden from the admin console (platform_settings table). Each read
	// path below consults this snapshot, so a change applies on the next use.
	settingsRuntime := settings.NewRuntime(settings.NewStore(pool), map[string]json.RawMessage{
		// Tools.
		settings.KeyHTTPToolAllowlist:     mustJSON(cfg.HTTPTool.Allowlist),
		settings.KeyHTTPToolTimeout:       mustJSON(int(cfg.HTTPTool.Timeout.Seconds())),
		settings.KeyHTTPToolMaxConcurrent: mustJSON(cfg.HTTPTool.MaxConcurrent),
		settings.KeyQueryDBDsns:           mustJSON(cfg.QueryDB.DSNS),
		settings.KeyQueryDBTimeout:        mustJSON(int(cfg.QueryDB.Timeout.Seconds())),
		settings.KeyRunCommandTimeout:     mustJSON(int(cfg.Sandbox.RunCommandTimeout.Seconds())),
		// Webhooks.
		settings.KeyWebhookURL:           mustJSON(cfg.Webhook.URL),
		settings.KeyWebhookTimeout:       mustJSON(int(cfg.Webhook.Timeout.Seconds())),
		settings.KeyWebhookRetries:       mustJSON(cfg.Webhook.Retries),
		settings.KeyWebhookSSRFAllowlist: mustJSON(cfg.Webhook.SSRFAllowlist),
		settings.KeyWebhookSigningSecret: mustJSON(cfg.Webhook.SigningSecret),
		// LLM.
		settings.KeySystemLang:             mustJSON(cfg.LLM.SystemLang),
		settings.KeyLLMContextWindow:       mustJSON(cfg.LLM.ContextWindow),
		settings.KeyLLMTemperature:         mustJSON(cfg.LLM.Temperature),
		settings.KeyLLMThinkingBudget:      mustJSON(cfg.LLM.ThinkingBudget),
		settings.KeyAgentMaxIterations:     mustJSON(cfg.LLM.MaxIterations),
		settings.KeyLLMStreamIdleTimeout:   mustJSON(int(cfg.LLM.StreamIdleTimeout.Seconds())),
		settings.KeyLLMRawLogDir:           mustJSON(cfg.LLM.RawLogDir),
		settings.KeyLLMRawLogRetentionDays: mustJSON(cfg.LLM.RawLogRetentionDays),
		// Sandbox.
		settings.KeySandboxNetwork:   mustJSON(cfg.Sandbox.Network),
		settings.KeySandboxLocalExec: mustJSON(cfg.Sandbox.LocalExec),
		// Permissions.
		settings.KeyPermissionReadOnly:      mustJSON(cfg.Permission.ReadOnly),
		settings.KeyPermissionSandboxWrite:  mustJSON(cfg.Permission.SandboxWrite),
		settings.KeyPermissionNetwork:       mustJSON(cfg.Permission.Network),
		settings.KeyPermissionExternalWrite: mustJSON(cfg.Permission.ExternalWrite),
		// Redaction.
		settings.KeyRedactEnabled:    mustJSON(cfg.Redact.Enabled),
		settings.KeyRedactStrategy:   mustJSON(cfg.Redact.Strategy),
		settings.KeyRedactCategories: mustJSON(cfg.Redact.Categories),
		// Subagents.
		settings.KeySubagentEnabled:       mustJSON(cfg.Subagent.Enabled),
		settings.KeySubagentMaxDepth:      mustJSON(cfg.Subagent.MaxDepth),
		settings.KeySubagentMaxTotal:      mustJSON(cfg.Subagent.MaxTotal),
		settings.KeySubagentMaxConcurrent: mustJSON(cfg.Subagent.MaxConcurrent),
		// Background tasks.
		settings.KeyDreamingEnabled:      mustJSON(cfg.Dreaming.Enabled),
		settings.KeyDreamingInterval:     mustJSON(int(cfg.Dreaming.Interval.Seconds())),
		settings.KeyDreamingMaxTokens:    mustJSON(cfg.Dreaming.MaxTokens),
		settings.KeyDreamingMaxFacts:     mustJSON(cfg.Dreaming.MaxFacts),
		settings.KeyDreamingMaxInsights:  mustJSON(cfg.Dreaming.MaxInsights),
		settings.KeyDreamingMaxSummaries: mustJSON(cfg.Dreaming.MaxSummaries),
		settings.KeyDreamingPurgeAfter:   mustJSON(int(cfg.Dreaming.PurgeAfter.Hours() / 24)),
		settings.KeyScheduleEnabled:      mustJSON(cfg.Schedule.Enabled),
		settings.KeyScheduleScanInterval: mustJSON(int(cfg.Schedule.ScanInterval.Seconds())),
		// HTTP layer.
		settings.KeyRateLimitRPS:   mustJSON(cfg.HTTP.RateLimitRPS),
		settings.KeyRateLimitBurst: mustJSON(cfg.HTTP.RateLimitBurst),
		// Workspace image retention (overrides WORKSPACE_RETENTION_DAYS live).
		settings.KeyWorkspaceRetentionDays: mustJSON(cfg.Workspace.RetentionDays),
		// Conversation retention (overrides CONVERSATION_RETENTION_DAYS live).
		settings.KeyConversationRetentionDays: mustJSON(cfg.Conversation.RetentionDays),
		// Audit-trail retention (overrides AUDIT_RETENTION_DAYS live).
		settings.KeyAuditRetentionDays: mustJSON(cfg.Audit.RetentionDays),
		// User image uploads (user-image-uploads quota; overrides
		// UPLOAD_MAX_FILES_PER_USER / UPLOAD_MAX_BYTES_PER_USER live).
		settings.KeyUploadMaxFilesPerUser: mustJSON(cfg.Upload.MaxFilesPerUser),
		settings.KeyUploadMaxBytesPerUser: mustJSON(cfg.Upload.MaxBytesPerUser),
		// Auth / SSO.
		settings.KeyPhoneSMSURL:     mustJSON(cfg.Phone.SMSURL),
		settings.KeyPhoneSMSTimeout: mustJSON(int(cfg.Phone.Timeout.Seconds())),
		// Integrations (MCP_SERVERS or the legacy SearXNG form; headers may
		// carry bearer tokens, so the runtime value is treated as a secret).
		settings.KeyMCPServers: mustJSON(initialMCPServers(cfg)),
	}, log)
	if err := settingsRuntime.Load(ctx); err != nil {
		log.Warn("platform settings load failed; env defaults in effect", "err", err)
	} else {
		log.Info("platform settings loaded", "keys", len(settings.Keys()))
	}
	// Multi-instance convergence (P2-6): reload the snapshot on an interval so
	// rows written by another gateway process (or the console on it) reach this
	// process without a restart or a local write. The loop stops when the root
	// ctx is cancelled.
	settingsRuntime.StartRefreshLoop(ctx, 30*time.Second)
	// Five-second settings sync (P2-7): one Watcher drives every component's
	// "re-read the runtime settings and apply" callback on a 5s cadence —
	// MCP servers, dreaming/schedule cadence, webhook policy, rate limiter.
	// Components register their callback where they are wired; the loop
	// starts once at the end of run().
	settingsSync := settings.NewWatcher()

	// Secret encryption at rest (SECRETS_MASTER_KEY): one encryptor shared by
	// every store that persists credentials — provider registry keys, inbound
	// webhook secrets, and TOTP seeds. Built before the wire phases so the
	// identity phase (which runs first) gets the same protection. A malformed
	// key fails the boot: silently booting unencrypted while the operator
	// believes keys are protected is worse than not starting.
	enc, err := buildEncryptor(cfg)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if enc == nil {
		log.Warn("SECRETS_MASTER_KEY unset: provider keys, webhook secrets and TOTP seeds stored PLAINTEXT; set it to enable encryption at rest")
	}

	// Auth surface (wire_identity.go): identity store/service/handler, the
	// credential sweep, the shared audit logger, OIDC SSO, phone SMS-OTP,
	// email password reset, and the admin bootstrap.
	d := &serverDeps{
		cfg: cfg, log: log, pool: pool, mux: mux,
		health: health, metrics: metrics,
		settings: settingsRuntime, settingsSync: settingsSync,
		enc: enc,
	}
	if err := d.wireIdentity(ctx); err != nil {
		return err
	}

	// Protected route tier (httpx.Router): auth — and any future per-route
	// concern (CSRF, encryption context, tenant resolution) — is applied ONCE to
	// the whole group at Mount, instead of each handler wrapping its own routes
	// in RequireAuth. Open routes (auth, oidc, healthz, metrics) stay on the
	// outer mux; a more specific pattern there beats the "/api/" subtree.
	protected := httpx.NewRouter(d.identityHandler.RequireAuth)
	d.protected = protected

	// Provider registry (wire_provider.go): DB-managed providers/models, the
	// secret encryptor, the raw LLM recorder and its retention sweep.
	if err := d.wireProviderRegistry(ctx); err != nil {
		return err
	}

	// Durable session runtime (wire_session.go): stores, run registry, sweeps,
	// the live broker (mem/Redis) and its drop metrics.
	if err := d.wireSessionRuntime(ctx); err != nil {
		return err
	}

	// Workspace images, user uploads, and the retention sweeps
	// (wire_workspace.go).
	d.wireWorkspace(ctx)

	// Sandbox backend (wire_sandbox.go): local/docker/off, the lifecycle
	// reaper, and the exec-enabled predicate.
	if err := d.wireSandbox(ctx); err != nil {
		return err
	}

	// Memory / skills / agent definitions / the chat context builder
	// (wire_skills.go).
	d.wireSkillsAndMemory()

	// Billing attribution + the budget gate (wire_usage.go).
	d.wireUsage()

	// Chat endpoint and every provider-dependent capability (wire_chat.go):
	// build an agent loop per request from the provider registry. The loop
	// factories resolve provider+model per request, so registry edits and team
	// reassignments take effect without a restart. A request that cannot be
	// resolved fails closed — there is no boot-time default adapter.
	if err := d.wireChat(ctx); err != nil {
		return err
	}

	// Management consoles (wire_consoles.go): admin console, skill management,
	// agent definitions, scheduled-task CRUD — all wired regardless of whether
	// a provider is configured.
	d.wireConsoles()

	// Mount the protected tier once: every authed route above is now behind the
	// group's middleware set, with a single wrap instead of one per route.
	protected.Mount(mux, "/api/")

	// OpenAPI contract enforcement (openapi-route-contract): the patterns the
	// protected tier ACTUALLY registered are checked against the contract
	// before the server accepts traffic. registeredRoutes is the authoritative
	// input for the spec test, but that test cannot see this assembly (it
	// lives here, in run()), so the same list is enforced at boot: a route
	// added in code without syncing openapi/routes.go + paths.go fails startup
	// instead of silently drifting. Open mux routes (identity/phone/oidc/meta)
	// are exempted inside the check — they are not recorded by the group.
	if err := openapi.VerifyAuthedRoutes(protected.Patterns()); err != nil {
		return err
	}

	// Serve the built frontend if present. The console is a client-side route,
	// so a deep link like /admin/users has no file behind it — spaHandler falls
	// back to index.html. API routes carry more specific patterns, which Go's
	// ServeMux prefers over this one, so they are unaffected.
	if cfg.Web.Dir != "" {
		// SPA must be registered method-less: "GET /" would conflict with the
		// all-methods "/api/" subtree (more general path, fewer methods). The
		// method guard lives in spaHandler instead.
		mux.Handle("/", spaHandler(cfg.Web.Dir))
	}

	srv := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      httpHandler(ctx, cfg, log, metrics, settingsRuntime, settingsSync, mux),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
	}

	// Start the 5-second settings sync: every registered callback (MCP
	// servers, dreaming/schedule cadence, webhook policy, rate limiter) now
	// runs on the shared watcher loop. All callbacks are registered by this
	// point, and the loop stops when the root ctx is cancelled.
	settingsSync.StartSync(ctx, 5*time.Second)

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

// clampPermissionDecision parses a runtime-settings permission value into a
// permission.Decision. Any value that is not a known allow/ask/deny is clamped
// to deny (fail-closed) and logged, so a corrupt or hand-edited setting can
// never silently open an execution gate.
func clampPermissionDecision(v, key string) permission.Decision {
	switch permission.Decision(v) {
	case permission.DecisionAllow, permission.DecisionAsk, permission.DecisionDeny:
		return permission.Decision(v)
	default:
		slog.Warn("invalid permission setting clamped to deny", "key", key, "value", v)
		return permission.DecisionDeny
	}
}

// reconnectMCP keeps trying to establish the MCP session until it succeeds or
// the server shuts down. The first failure is logged as a warning so an
// unreachable/misconfigured server is visible; subsequent retries back off
// quietly and success is announced once tools are registered. It runs for the
// process lifetime (ctx is the signal context) and only stops on shutdown.
func reconnectMCP(ctx context.Context, c *mcp.Client, log *slog.Logger) {
	backoff := time.Second
	first := true
	for {
		if err := c.Connect(ctx); err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			if first {
				log.Warn("mcp connect failed; retrying in background", "server", c.Server(), "err", err)
				first = false
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		log.Info("mcp connected", "server", c.Server(), "tools", len(c.Tools()))
		return
	}
}

// usageObserver returns an AfterRun hook that reports a root run's aggregate
// token usage into the platform metrics (nowhere_llm_tokens_total), labelled
// with the resolved provider vendor and model. It is attached ONLY to loops
// built by buildLoop (chat / scheduled / inbound); the subagent factory builds
// children separately and their usage folds into the root run's UsageScope,
// so nothing is counted twice. The hook fires once per run on every exit path
// (done, error, cancel).
func usageObserver(vendor, model string, metrics *observability.Metrics) agent.Middleware {
	return agent.UsageHookFunc(func(_ context.Context, s *agent.RunState) error {
		metrics.RecordTokens(vendor, model, "input", s.Usage.InputTokens)
		metrics.RecordTokens(vendor, model, "output", s.Usage.OutputTokens)
		metrics.RecordTokens(vendor, model, "cache_read", s.Usage.CacheReadTokens)
		metrics.RecordTokens(vendor, model, "cache_write", s.Usage.CacheWriteTokens)
		return nil
	})
}

// validDBName reports whether name is a safe query_db database identifier
// ([a-z0-9_-], no dots, slashes, or whitespace — it is used as a map key and
// surfaced in tool output).
func validDBName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// validDBDSN reports whether dsn is a scheme the query_db tool can open.
func validDBDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") || strings.HasPrefix(dsn, "mysql://")
}

// mustJSON encodes v as a settings value; the values are constants from
// config at boot, so an encoding error cannot happen.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// initialMCPServers resolves the boot MCP server list: MCP_SERVERS when set,
// otherwise the legacy MCP_ENABLED + MCP_SEARXNG_URL SearXNG integration
// mapped to a single "searxng" server. It seeds the runtime default AND
// builds the boot manager, so both stay in lockstep.
func initialMCPServers(cfg config.Config) string {
	if cfg.MCP.Servers != "" {
		return cfg.MCP.Servers
	}
	if cfg.MCP.Enabled {
		b, err := json.Marshal([]mcp.ServerConfig{{Name: "searxng", URL: cfg.MCP.SearxngURL}})
		if err != nil {
			return ""
		}
		return string(b)
	}
	return ""
}

// noProviderAdapter fails every generation with the resolver's ErrNoProvider.
// It is the loop's adapter for a chat request that could not be resolved (empty
// or misconfigured registry), so the client receives a clean error frame
// instead of a hang or a panic — the chatapi LoopFactory cannot surface an
// error, so the loop carries the failure instead.
type noProviderAdapter struct{}

func (noProviderAdapter) Name() string { return "none" }
func (noProviderAdapter) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	return nil, providerreg.ErrNoProvider
}

// spaHandler serves static files from dir, falling back to index.html for
// paths that do not name a FILE — including real directories: an existing
// subdirectory must never be served as a FileServer directory listing, it
// falls back to the app shell like any client route. That fallback is what
// makes client-side routes (/admin/users and friends) survive a reload or a
// shared link — a plain FileServer answers 404 for them.
//
// The fallback deliberately does NOT apply to /api/: those routes are
// registered with more specific patterns, which Go 1.22+ ServeMux matches in
// preference to "GET /". A request that reaches here asking for a MISSING
// file — a stale hashed asset after a deploy (a path with a file extension)
// — gets an explicit 404, not the shell: the browser would otherwise execute
// the HTML as a module script and render a silent blank screen. Extension-less
// paths (client routes like /chat/xxx) still get the shell.
//
// The shell (index.html) is served with Cache-Control: no-cache so browsers
// revalidate it on every navigation: after a deploy, a heuristically cached
// shell would be reused, its stale asset hashes would 404, and the fallback
// would hand back the same stale shell — the classic "hard refresh required"
// symptom. Hashed static assets keep the FileServer's default caching.
// spaCSP is the Content-Security-Policy for the SPA. The build ships no
// inline scripts — dist/index.html's module tag is a pure external
// /assets/*.js — so script-src 'self' blocks injected script execution (the
// Bearer token lives in localStorage, so a strict script policy is the
// compensating control for an XSS). style-src keeps 'unsafe-inline': the app
// uses inline style attributes and libraries inject <style> blocks, so a
// strict style-src would break the UI. img-src admits data:/blob: for inline
// SVG data and client-side object URLs.
const spaCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; font-src 'self' data:; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

func spaHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", spaCSP)
		// Reject traversal before touching the filesystem: path.Clean on a
		// rooted path cannot escape, and http.ServeFile would reject it anyway,
		// but checking here keeps the stat below honest.
		clean := path.Clean("/" + r.URL.Path)
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clean))); err == nil && clean != "/" && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		// A path that names a FILE (has a file extension) is an asset
		// reference: a stale hashed .js/.css after a deploy, or a typo. Answer
		// 404 rather than the shell — the browser would execute the HTML as a
		// module script, a silent blank screen with zero server-side signal.
		// Extension-less paths (client routes like /admin/users, directories)
		// still get the shell.
		if filepath.Ext(clean) != "" {
			http.NotFound(w, r)
			return
		}
		// The shell — for "/" and every extension-less fallback (client
		// routes). Must be revalidated, never heuristically cached.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}

// httpHandler composes the inbound middleware stack around the mux via Chain,
// outermost first. Order invariants:
//
//   - request-id outermost: an id is near-free (one random read), and giving
//     EVERY request one — including requests the limiter is about to reject —
//     keeps throttled floods traceable instead of anonymous.
//   - access-log next: one line per completed request (status/latency/ttfb/
//     bytes, 499 on client disconnect), so rejected requests are also seen.
//   - rate-limit (P1-1) before metrics: a flood must not churn metric series;
//     probes are opted out so monitoring stays up during a flood. Limiting is
//     disabled unless both HTTP_RATE_LIMIT_RPS and _BURST are set.
//   - metrics then recovery, innermost around the mux: a panic is recovered,
//     logged with a stack, and answered 500 — and because recovery sits inside
//     metrics, that 500 is counted like any other status.
//
// rateLimitKey derives the bucket key for one request: the bearer session
// token when one is presented, else the client IP. Tokens are opaque (there is
// no parseable JWT subject — resolving one would need a DB lookup in the hot
// path), but each token is unique per session and unguessable, so authenticated
// users behind one NAT no longer share a bucket, and a spoofed Authorization
// header cannot hijack a bucket. A missing/malformed bearer falls back to the
// IP key, so anonymous flood smoothing is unchanged.
//
// Like every limiter in the platform (the open-endpoint per-IP floor here, and
// the login/TOTP/signup/OTP throttles in internal/identity), the bucket map is
// in-memory PER INSTANCE: N gateways multiply each client's allowance by N.
// Multi-instance deployments should front the auth surface with a shared
// reverse-proxy limiter or pin auth to one instance (documented in AGENTS.md).
func rateLimitKey(r *http.Request, proxies *trustedproxy.Set) string {
	// Never throttle probes: a flooded API must not blind the operator.
	if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
		return ""
	}
	if tok := identity.BearerToken(r); tok != "" {
		sum := sha256.Sum256([]byte(tok))
		return "auth:" + hex.EncodeToString(sum[:])
	}
	return quota.ClientIPKey(r, proxies)
}

// openEndpointRPS/openEndpointBurst is the per-IP floor on the
// UNAUTHENTICATED open endpoints that have no other built-in backstop: signup
// (a flood burns bcrypt cost and can fill the user table), the phone and email
// request-code (gateway spend / code minting), the OIDC callback, and the
// inbound trigger (HMAC-verified but still a public POST — a flood burns HMAC
// checks and dispatches). 10 rps per IP is far above any human flow but stops
// a single source from hammering. It is deliberately separate from the global
// limiter: a deployment that disables HTTP_RATE_LIMIT_RPS/BURST (or has not
// set them) still has a bounded edge.
const (
	openEndpointRPS   = 10
	openEndpointBurst = 20
)

// sandboxStopGrace is how long a finished run's sandbox stays resumable after
// its last run terminates before the hourly sweep destroys it (one hourly
// sweep window: a container lives 1-2 hours past the session's last run).
const sandboxStopGrace = time.Hour

// uploadStaleTTL is how long an upload row may sit UNREFERENCED before the
// hourly upload sweep deletes it (row + blob). The frontend uploads an image
// the moment the user picks it — sent or not — and the metadata row alone
// counts against their quota, so staged-but-never-sent images would otherwise
// occupy quota forever. Chosen conservatively: 30 days matches the default
// WORKSPACE_RETENTION_DAYS window, and any message reference (of any age)
// always keeps the upload.
const uploadStaleTTL = 30 * 24 * time.Hour

// openEndpointKey is the floor limiter's bucket key: the client IP for the
// unauthenticated open endpoints, "" (opt-out) for everything else. The
// inbound trigger is a prefix match (the endpoint carries a path param);
// /api/me/inbound/* is NOT covered — it is bearer-authed and already keyed by
// token hash in the global limiter.
func openEndpointKey(r *http.Request, proxies *trustedproxy.Set) string {
	if strings.HasPrefix(r.URL.Path, "/api/inbound/") {
		return quota.ClientIPKey(r, proxies)
	}
	switch r.URL.Path {
	case "/api/auth/signup", "/api/auth/phone/request-code", "/api/auth/email/reset-code", "/auth/oidc/callback":
		return quota.ClientIPKey(r, proxies)
	}
	return ""
}

func httpHandler(ctx context.Context, cfg config.Config, log *slog.Logger, metrics *observability.Metrics, settingsRuntime *settings.Runtime, settingsSync *settings.Watcher, mux *http.ServeMux) http.Handler {
	proxies := trustedproxy.New(cfg.HTTP.TrustedProxyCIDRs)
	limiter := quota.NewRateLimiter(cfg.HTTP.RateLimitRPS, cfg.HTTP.RateLimitBurst,
		func(r *http.Request) string {
			return rateLimitKey(r, proxies)
		})
	// Live retune: pick up the runtime settings (0/0 = disabled); existing
	// buckets converge within the limiter's sweep TTL. The settings watcher
	// keeps the rate in sync with the admin console, so retuning
	// rate_limit_rps / rate_limit_burst needs no restart. rps is a KindFloat
	// key — read via Float64, or a fractional value (e.g. 2.5) would unmarshal
	// into int as 0 and silently disable the limiter.
	limiter.SetRate(
		settingsRuntime.Float64(settings.KeyRateLimitRPS),
		settingsRuntime.Int(settings.KeyRateLimitBurst),
	)
	settingsSync.Add(func() {
		limiter.SetRate(
			settingsRuntime.Float64(settings.KeyRateLimitRPS),
			settingsRuntime.Int(settings.KeyRateLimitBurst),
		)
	})
	if settingsRuntime.Float64(settings.KeyRateLimitRPS) <= 0 || settingsRuntime.Int(settings.KeyRateLimitBurst) <= 0 {
		log.Warn("no global per-client rate limit in effect (HTTP_RATE_LIMIT_RPS/BURST unset; enable from the admin console): open auth + inbound endpoints keep their per-IP floor, everything else is unlimited",
			"open_endpoint_rps", openEndpointRPS)
	}
	// Per-IP floor for the unauthenticated open endpoints (see the consts).
	// Keyed on the path — only those routes consume a bucket; everything else
	// opts out. Sits OUTSIDE the global limiter so the floor holds even when
	// the global limiter is disabled (the boot warning above).
	openLimiter := quota.NewRateLimiter(openEndpointRPS, openEndpointBurst, func(r *http.Request) string {
		return openEndpointKey(r, proxies)
	})
	rateLimits := func(h http.Handler) http.Handler {
		return observability.Chain(h, openLimiter.Middleware, limiter.Middleware)
	}
	return observability.StandardStack(mux, log, metrics, rateLimits)
}

// buildEncryptor constructs the secret encryptor from config, or nil when no
// master key is set (encryption disabled, plaintext fallback). An error means a
// key WAS provided but is malformed — that is a hard failure, because silently
// ignoring a mis-set SECRETS_MASTER_KEY would boot with keys unprotected while
// the operator believes they are encrypted.
func buildEncryptor(cfg config.Config) (*secrets.Encryptor, error) {
	if cfg.Secrets.MasterKey == "" {
		return nil, nil
	}
	return secrets.NewSingle([]byte(cfg.Secrets.MasterKey))
}

// newWebhookGuard builds the SSRF guard for webhook delivery from the
// allowlist entries. It always returns a guard: an empty list yields the
// strict guard (public targets only), so the default configuration still
// screens private/loopback targets — a nil guard is never the outcome of an
// empty allowlist. Only a malformed CIDR is an error; callers decide what
// that means (boot: fail; runtime: keep the previous guard).
func newWebhookGuard(entries []string) (*webhook.Guard, error) {
	return webhook.NewGuard(entries, nil)
}

// splitComma splits a comma-separated list, trimming whitespace and dropping
// empty entries.
func splitComma(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
