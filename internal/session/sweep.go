package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// SweepEndedConversations is the conversation retention sweep (P2-8
// no-data-hard-delete): it hard-deletes every session the lister reports as
// ended before cutoff, bounded by limit per page so one pass cannot grow
// unbounded. The sessions FK cascade removes each session's runs, messages,
// run_events, approvals, suspended batches, run_steps and usage_records — the
// same hard delete the admin session purge performs. Best-effort per session:
// a failure is logged and the sweep continues with the next id. Returns the
// number of sessions removed.
//
// cleanup, when non-nil, is invoked after each successful delete with the
// deleted session id — the wiring point for per-session side storage the FK
// cascade cannot reach (the workspace image dir; the admin purge deletes it
// explicitly for the same reason, and the image retention sweep cannot: it
// lists sessions from the DB, which no longer has the row). A cleanup failure
// is logged, never allowed to fail or retry the delete.
//
// Pagination is keyset exactly like the workspace image sweep: after each page
// the last id becomes the next call's afterID cursor. Deleting the sessions we
// process moves the rows themselves, so the scan advances naturally; as a
// guard against a lister that ignores the cursor and repeats a page, a page
// whose last id equals the cursor passed in aborts the pass instead of
// looping. Only ENDED sessions older than the cutoff are ever listed, so
// active conversations and the retention grace window are untouched.
func SweepEndedConversations(ctx context.Context, log *slog.Logger, store Store, cutoff time.Time, limit int, cleanup func(sessionID string)) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	var removed int
	var cursor string
	for {
		ids, err := store.ListEndedSessionsEndedBefore(ctx, cutoff, cursor, limit)
		if err != nil {
			return removed, fmt.Errorf("list ended sessions: %w", err)
		}
		if len(ids) == 0 {
			return removed, nil
		}
		if ids[len(ids)-1] == cursor {
			if log != nil {
				log.Warn("conversation sweep: lister did not advance past cursor, aborting pass", "cursor", cursor)
			}
			return removed, nil
		}
		for _, id := range ids {
			if err := store.DeleteSession(ctx, id); err != nil {
				if log != nil {
					log.Warn("conversation sweep: delete session failed", "session", id, "err", err)
				}
				continue
			}
			removed++
			if cleanup != nil {
				cleanup(id)
			}
		}
		cursor = ids[len(ids)-1]
		if len(ids) < limit {
			return removed, nil
		}
	}
}
