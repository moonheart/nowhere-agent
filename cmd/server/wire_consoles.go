package main

import (
	"context"
	"fmt"

	"nowhere-agent/internal/adminapi"
	"nowhere-agent/internal/agentdefapi"
	"nowhere-agent/internal/export"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/schedule"
	"nowhere-agent/internal/scheduleapi"
	"nowhere-agent/internal/skillapi"
	"nowhere-agent/internal/usage"
	"nowhere-agent/internal/webhook"
)

// wire_consoles.go — the management consoles: admin console, skill management,
// agent-definition management, and scheduled-task CRUD. All are wired
// regardless of whether an LLM provider is configured (only FIRING runs needs
// one), which is why this phase reads d.dreamRunner / d.schedTrigger as
// possibly-nil fields instead of sharing the chat phase's scope. Extracted
// verbatim from run() (see deps.go).

func (d *serverDeps) wireConsoles() {
	log, protected := d.log, d.protected

	// Management console (admin-console): self-service, team, and platform
	// routes, all behind the same auth middleware the chat endpoint uses. It is
	// registered outside the provider branch so the console stays reachable on
	// a deployment with no provider configured.
	adminHandler := adminapi.NewHandler(d.identitySvc, usage.NewStore(d.pool), d.memPort).
		WithQuotas(quota.NewStore(d.pool)).
		WithProviders(d.provStore).
		WithUploads(d.uploadSvc).
		WithDreaming(d.dreamRunner).
		WithAudit(d.auditLogger).
		WithExporter(export.New(d.pool, d.messageStore, d.memPort, d.uploadSvc, schedule.NewPGStore(d.pool))).
		WithWebhookDeliveries(webhook.NewDeliveryStore(d.pool)).
		WithRuntimeSettings(d.settings).
		WithPhoneThrottle(d.phoneThrottle).
		// Platform purge (no-data-hard-delete): hard-delete routes for
		// sessions and image cleanup on user deletion. The shared run registry
		// stops an in-flight run before the session row goes.
		WithPurge(d.sessionStore, d.imageStore, d.runRegistry)
	adminHandler.RegisterAuthed(protected)
	log.Info("admin console endpoints enabled (auth required)")

	// Skill management (skill-console): user/team/system skill CRUD + versioning,
	// behind the same auth middleware. Registered alongside the admin console.
	skillapi.NewHandler(d.identitySvc, d.skillStore).RegisterAuthed(protected)
	log.Info("skill management endpoints enabled (auth required)")

	// Agent-definition management (persist-agent-defs): user/team/system
	// definition CRUD over the same PG store the spawn resolver reads, behind
	// the same auth middleware. The runnable check mirrors the run registry's
	// run_skill_script registration rule (exec enabled + some visible skill has
	// scripts), so a definition declaring unusable skills is flagged on write.
	skillsRunnable := func(ctx context.Context, scopes []identity.ScopeRef) bool {
		if !d.execEnabledFor() {
			return false
		}
		l0, err := d.skillEngine.LoadL0(ctx, scopes)
		if err != nil {
			return false
		}
		for _, meta := range l0 {
			if len(meta.Scripts) > 0 {
				return true
			}
		}
		return false
	}
	agentdefapi.NewHandler(d.identitySvc, d.agentDefPG, skillsRunnable).RegisterAuthed(protected)
	log.Info("agent definition endpoints enabled (auth required)")

	// Scheduled-task CRUD (scheduled-tasks): self-service management of recurring
	// agent runs. Registered outside the provider branch so tasks can be managed
	// on a deployment with no LLM; only firing needs a provider, so run-now is
	// wired to the trigger when one was built above and answers 503 otherwise.
	scheduleapi.NewHandler(schedule.NewPGStore(d.pool)).WithRunner(d.schedTrigger).
		WithTargetValidator(func(ctx context.Context, userID, sessionID string) error {
			sess, err := d.sessionRuntime.GetSession(ctx, sessionID)
			if err != nil {
				return err
			}
			if sess.UserID != userID {
				return fmt.Errorf("session %s is not owned by %s", sessionID, userID)
			}
			return nil
		}).RegisterAuthed(protected)
	log.Info("scheduled-task endpoints enabled (auth required)")
}
