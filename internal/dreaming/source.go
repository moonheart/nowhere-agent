package dreaming

import (
	"context"
	"sort"

	"nowhere-agent/internal/session"
)

// undreamedScanner is the session store's native eligibility query
// (PGStore.ListUndreamedSessions answers it in SQL). The in-memory session
// store cannot (it can't join messages), so StoreSource falls back to a
// per-session in-memory filter when this is absent.
type undreamedScanner interface {
	ListUndreamedSessions(ctx context.Context) ([]session.Session, error)
}

// sessionWatermarks is the slice of the session store the worker needs for the
// incremental watermark model (capability-gap K1). *session.PGStore and
// *session.MemStore satisfy it.
type sessionWatermarks interface {
	DreamedSeq(ctx context.Context, id string) (int64, error)
	MarkDreamedSeq(ctx context.Context, id string, seq int64) error
}

// StoreSource is the production EpisodeSource: it drives the incremental
// watermark model off the session store (sessions.dreamed_seq, migration
// 000009) and reads the episode tail from the message store (MessagesAfter
// returns exactly the messages beyond a watermark, seq-ordered).
type StoreSource struct {
	sessions sessionWatermarks
	scanner  undreamedScanner // optional: nil → per-session fallback
	messages session.MessageStore
}

// NewStoreSource wires an EpisodeSource over the session and message stores. If
// sessions also implements ListUndreamedSessions (PGStore), eligibility uses it;
// otherwise it is computed per session from the watermarks + message store. The
// in-memory *session.MemStore implements the method but only to report it as
// unsupported (it cannot join messages), so it is excluded here and takes the
// fallback path.
func NewStoreSource(sessions sessionWatermarks, messages session.MessageStore) *StoreSource {
	src := &StoreSource{sessions: sessions, messages: messages}
	if sc, ok := sessions.(undreamedScanner); ok {
		if _, isMem := sessions.(*session.MemStore); !isMem {
			src.scanner = sc
		}
	}
	return src
}

// PendingSessions returns sessions with undreamed messages (any status),
// each carrying the watermark to resume from.
func (s *StoreSource) PendingSessions(ctx context.Context) ([]PendingSession, error) {
	if s.scanner != nil {
		sessions, err := s.scanner.ListUndreamedSessions(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]PendingSession, 0, len(sessions))
		for _, sess := range sessions {
			wm, err := s.sessions.DreamedSeq(ctx, sess.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, PendingSession{Session: sess, Seq: wm})
		}
		return out, nil
	}
	return s.pendingFallback(ctx)
}

// pendingFallback computes eligibility without a native store query: it lists
// the store's sessions and keeps those that actually have messages beyond
// their watermark. Used by the in-memory session store (and any store without
// ListUndreamedSessions).
func (s *StoreSource) pendingFallback(ctx context.Context) ([]PendingSession, error) {
	type sessionLister interface {
		Sessions() []session.Session
	}
	lister, ok := s.sessions.(sessionLister)
	if !ok {
		// No way to enumerate sessions at all: nothing to dream.
		return nil, nil
	}
	all := lister.Sessions()
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.Before(all[j].UpdatedAt) })
	var out []PendingSession
	for _, sess := range all {
		wm, err := s.sessions.DreamedSeq(ctx, sess.ID)
		if err != nil {
			return nil, err
		}
		msgs, err := s.messages.MessagesAfter(ctx, sess.ID, wm)
		if err != nil {
			return nil, err
		}
		if len(msgs) > 0 {
			out = append(out, PendingSession{Session: sess, Seq: wm})
		}
	}
	return out, nil
}

// Episodes returns the session's messages beyond the watermark, ordered by seq.
func (s *StoreSource) Episodes(ctx context.Context, sessionID string, afterSeq int64) ([]session.StoredMessage, error) {
	return s.messages.MessagesAfter(ctx, sessionID, afterSeq)
}

// MarkProcessed advances the session's dreamed watermark.
func (s *StoreSource) MarkProcessed(ctx context.Context, sessionID string, newSeq int64) error {
	return s.sessions.MarkDreamedSeq(ctx, sessionID, newSeq)
}
