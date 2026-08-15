package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"nowhere-agent/internal/agentdef"
	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/chatapi"
	"nowhere-agent/internal/config"
	"nowhere-agent/internal/dreaming"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/observability"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/schedule"
	"nowhere-agent/internal/secrets"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/settings"
	"nowhere-agent/internal/skill"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/usage"
	"nowhere-agent/internal/workspace"
)

// serverDeps carries the wiring state between the phases of run(). It exists
// because run() used to be one 2400-line function whose "provider branch" was
// a bare lexical block closing over ~30 variables — nothing could statically
// answer "what breaks when no provider is configured", and every new
// capability made the closure graph harder to verify. Each wire* method in
// the wire_*.go files fills in the fields it produces and may read the fields
// earlier phases produced; the call order in run() IS the dependency order.
//
// Field grouping follows the phase order. Everything is set exactly once per
// boot, before the HTTP server starts accepting traffic, so no field needs
// synchronization.
type serverDeps struct {
	// Boot phase (set by run itself before any wire phase).
	cfg          config.Config
	log          *slog.Logger
	pool         *sql.DB
	mux          *http.ServeMux
	protected    *httpx.Router
	health       *observability.Healthz
	metrics      *observability.Metrics
	settings     *settings.Runtime
	settingsSync *settings.Watcher

	// wireIdentity.
	identityStore   *identity.Store
	identitySvc     *identity.Service
	identityHandler *identity.Handler
	auditLogger     *audit.Logger
	phoneThrottle   *identity.OTPThrottler

	// wireProviderRegistry.
	provStore    *providerreg.PGStore
	enc          *secrets.Encryptor
	provResolver *providerreg.Resolver
	recorder     *provider.RawRecorder

	// wireSessionRuntime.
	sessionStore   *session.PGStore
	sessionRuntime *session.Runtime
	runRegistry    *session.RunRegistry
	messageStore   *session.PGMessageStore

	// wireWorkspace.
	imageStore *workspace.ImageStore
	uploadSvc  *upload.Service

	// wireSandbox.
	wsRoot      string
	sandboxPort sandbox.Port
	sandboxMgr  *sandbox.Manager

	// wireSkillsAndMemory.
	memPort     *memory.PGPort
	skillStore  *skill.PGStore
	skillEngine *skill.Engine
	agentDefPG  *agentdef.PGStore
	ctxBuilder  chatapi.ContextBuilder

	// Usage / quota (wireUsage).
	usageStore     *usage.Store
	budgetChecker  *quota.Checker
	teamAttributor func(ctx context.Context, userID string) string

	// wireChat (the former "provider branch"). dreamRunner and schedTrigger
	// stay nil when no platform provider is configured; the consoles read
	// them either way.
	dreamRunner  *dreaming.Runner
	schedTrigger *schedule.Trigger
}
