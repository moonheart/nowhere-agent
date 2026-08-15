package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/providerreg"
	"nowhere-agent/internal/settings"
)

// wire_provider.go — the DB-managed provider registry, the secret encryptor,
// the raw LLM recorder and its retention sweep. Extracted verbatim from
// run() (see deps.go).

func (d *serverDeps) wireProviderRegistry(ctx context.Context) error {
	cfg, log := d.cfg, d.log

	// Provider registry (change provider-registry): DB-managed LLM providers and
	// models replace the env-var model selection (LLM_*/VISION_*) and the
	// deprecated team_api_keys mechanism. Teams select a system or team-owned
	// provider; every decision is resolved per request, so registry edits and
	// reassignments take effect without a restart.
	d.provStore = providerreg.NewPGStore(d.pool)
	enc, err := buildEncryptor(cfg)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	d.enc = enc
	if enc != nil {
		d.provStore.WithEncryption(enc)
		log.Info("provider registry keys encrypted at rest (AES-256-GCM)")
	} else {
		log.Warn("SECRETS_MASTER_KEY unset: provider registry keys stored PLAINTEXT; set it to enable encryption at rest")
	}
	// The resolver caches resolutions for a few seconds (WithCacheTTL): one
	// chat submission resolves the caller's target, the tool binder resolves
	// again for view_image, and the model picker lists the provider's models —
	// each a handful of PG round trips over data that only changes on an
	// operator edit. The TTL keeps console edits effectively live.
	d.provResolver = providerreg.NewResolver(d.provStore).WithCacheTTL(5 * time.Second)
	d.recorder = provider.NewRawRecorder(cfg.LLM.RawLogDir)
	if d.recorder.Enabled() {
		log.Info("recording raw LLM request/response", "dir", cfg.LLM.RawLogDir)
	}
	// Raw LLM log retention (LLM_RAW_LOG_RETENTION_DAYS): request/response
	// files otherwise accumulate without bound; an hourly pass deletes files
	// older than the retention window (<= 0 disables), mirroring the other
	// sweeps. The root is re-read per pass, so an admin-console retarget still
	// sweeps the current dir; the retention window itself is runtime-tunable
	// (llm_raw_log_retention_days) via the 5s settings sync below, so turning
	// retention on/off needs no restart. The sweeper runs regardless and
	// skips passes while the window is <= 0, so enabling it at runtime works
	// even when the boot value disabled it.
	var rawLogRetention atomic.Int64
	rawLogRetention.Store(int64(cfg.LLM.RawLogRetentionDays))
	hourlySweep(ctx, log, "raw log retention", func() error {
		days := rawLogRetention.Load()
		if days <= 0 {
			return nil
		}
		cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
		removed, err := d.recorder.Sweep(cutoff)
		if err != nil {
			return err
		}
		if removed > 0 {
			log.Info("raw log retention sweep removed files", "count", removed)
		}
		return nil
	})
	d.settingsSync.Add(func() {
		rawLogRetention.Store(int64(d.settings.Int(settings.KeyLLMRawLogRetentionDays)))
	})
	if _, err := d.provResolver.ResolveForTeam(ctx, ""); err != nil {
		log.Warn("no platform provider configured; chat/schedule fail until a provider is added (see the admin console)")
	}
	return nil
}
