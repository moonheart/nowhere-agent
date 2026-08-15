package main

import (
	"context"
	"sync/atomic"
	"time"

	"nowhere-agent/internal/session"
	"nowhere-agent/internal/settings"
	"nowhere-agent/internal/upload"
	"nowhere-agent/internal/workspace"
)

// wire_workspace.go — the workspace image store, the user-level upload service,
// and the retention sweeps (ended-session images, upload orphans, conversation
// retention). Extracted verbatim from run() (see deps.go).

func (d *serverDeps) wireWorkspace(ctx context.Context) {
	cfg, log := d.cfg, d.log

	// Workspace image store: image payloads referenced by messages live as
	// WebP files under a per-session dir; the messages table holds pointers.
	if cfg.Workspace.Dir != "" {
		d.imageStore = workspace.NewImageStore(cfg.Workspace.Dir)
	}
	// User-level image uploads (change user-image-uploads): session-independent
	// uploads so a brand-new conversation's first message can carry an image.
	// Blob + metadata index are wired to the chat handler (upload/serve) and the
	// console (/api/me/uploads). Requires the image store; without a workspace
	// dir the routes answer 503.
	if d.imageStore != nil {
		// Per-user upload quota, read live from the runtime settings so an
		// admin-console retune applies without a restart.
		d.uploadSvc = upload.NewService(upload.NewPGStore(d.pool), d.imageStore, func() upload.Quota {
			return upload.Quota{
				MaxFiles: d.settings.Int(settings.KeyUploadMaxFilesPerUser),
				MaxBytes: int64(d.settings.Int(settings.KeyUploadMaxBytesPerUser)),
			}
		})
		// Retention sweep (P2-8): an ended session's images are unreachable, so
		// an hourly pass deletes image dirs of sessions ended more than
		// WORKSPACE_RETENTION_DAYS ago (<= 0 disables). Only the session's own
		// image files are removed — a shared sandbox workspace, the upload
		// scope, and active sessions are never touched. One bounded scan per
		// tick, non-blocking, best-effort. The retention window is
		// runtime-tunable (workspace_retention_days) via the 5s settings sync
		// below, mirroring the raw-log sweep: the loop runs regardless and
		// skips passes while the window is <= 0, so enabling retention at
		// runtime works even when the boot value disabled it.
		var imageRetention atomic.Int64
		imageRetention.Store(int64(cfg.Workspace.RetentionDays))
		hourlySweep(ctx, log, "image retention", func() error {
			days := imageRetention.Load()
			if days <= 0 {
				return nil
			}
			cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
			removed, err := workspace.SweepEndedSessionImages(ctx, log, d.imageStore,
				d.sessionStore.ListEndedSessionsEndedBefore, cutoff, 200)
			if err != nil {
				return err
			}
			if removed > 0 {
				log.Info("image retention sweep removed session images", "count", removed)
			}
			return nil
		})
		d.settingsSync.Add(func() {
			imageRetention.Store(int64(d.settings.Int(settings.KeyWorkspaceRetentionDays)))
		})
		// Upload-orphan sweep (resource leak): Delete removes the uploads row
		// BEFORE the blob, so a blob-store hiccup leaves a terminal orphan
		// (ErrBlobRemovalFailed — a retry answers ErrNotFound) that no API path
		// can ever reclaim. An hourly pass deletes blobs that have no metadata
		// row AND no message reference — unambiguous garbage only; a row or a
		// reference keeps the blob. The same pass also reclaims STALE staged
		// uploads (row + blob, older than uploadStaleTTL, never referenced):
		// the frontend uploads an image the moment the user picks it, sent or
		// not, and the metadata row alone counts against their quota.
		hourlySweep(ctx, log, "upload orphan", func() error {
			removed, err := d.uploadSvc.SweepOrphans(ctx, log, time.Now().UTC().Add(-uploadStaleTTL))
			if err != nil {
				return err
			}
			if removed > 0 {
				log.Info("upload orphan sweep removed blobs", "count", removed)
			}
			return nil
		})
	}

	// Conversation retention (P2-8 no-data-hard-delete): without a policy the
	// message table grows without bound — images get a retention window but the
	// conversation body never did. An hourly pass hard-deletes sessions ended
	// more than CONVERSATION_RETENTION_DAYS ago (<= 0 disables), cascading over
	// runs/messages/run_events/approvals exactly like the admin session purge.
	// Only ENDED sessions past the window are eligible — active conversations
	// and the grace period are never touched. The retention window is
	// runtime-tunable (conversation_retention_days) via the 5s settings sync
	// below, mirroring the image/raw-log sweeps: the loop runs regardless and
	// skips passes while the window is <= 0, so enabling retention at runtime
	// works even when the boot value disabled it.
	//
	// The deleted session's image dir is reclaimed inline (cleanup hook), the
	// way the admin purge does: once the session row is gone the image
	// retention sweep can never list it, so its dir would otherwise orphan.
	var conversationRetention atomic.Int64
	conversationRetention.Store(int64(cfg.Conversation.RetentionDays))
	hourlySweep(ctx, log, "conversation retention", func() error {
		days := conversationRetention.Load()
		if days <= 0 {
			return nil
		}
		cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
		removed, err := session.SweepEndedConversations(ctx, log, d.sessionStore, cutoff, 50, func(sessionID string) {
			if d.imageStore == nil {
				return
			}
			if _, err := d.imageStore.DeleteSessionImages(sessionID); err != nil {
				log.Warn("conversation retention sweep: session image cleanup failed", "session", sessionID, "err", err)
			}
		})
		if err != nil {
			return err
		}
		if removed > 0 {
			log.Info("conversation retention sweep removed sessions", "count", removed)
		}
		return nil
	})
	d.settingsSync.Add(func() {
		conversationRetention.Store(int64(d.settings.Int(settings.KeyConversationRetentionDays)))
	})
}
