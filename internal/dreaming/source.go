package dreaming

import (
	"context"

	"nowhere-agent/internal/session"
)

// endedUndreamed is the slice of the session store the worker's eligibility
// scan needs. *session.PGStore (and the in-memory *session.MemStore) satisfy it
// (capability-gap K1). The narrow interface keeps the dreaming package free of
// a dependency on the full session.Store surface.
type endedUndreamed interface {
	ListEndedUndreamed(ctx context.Context) ([]session.Session, error)
	MarkDreamed(ctx context.Context, id string) error
}

// StoreSource is the production EpisodeSource: it reads eligibility and the
// dreamed marker from the session store (backed by sessions.dreamed_at,
// migration 000008) and the episode content from the message store
// (MessagesFor is exactly the per-session, seq-ordered record Episodes wants).
type StoreSource struct {
	sessions endedUndreamed
	messages session.MessageStore
}

// NewStoreSource wires an EpisodeSource over the session and message stores.
func NewStoreSource(sessions endedUndreamed, messages session.MessageStore) *StoreSource {
	return &StoreSource{sessions: sessions, messages: messages}
}

// EndedSessions returns ended sessions not yet dreamed over, oldest first.
func (s *StoreSource) EndedSessions(ctx context.Context) ([]session.Session, error) {
	return s.sessions.ListEndedUndreamed(ctx)
}

// Episodes returns the session's persisted messages, ordered by seq.
func (s *StoreSource) Episodes(ctx context.Context, sessionID string) ([]session.StoredMessage, error) {
	return s.messages.MessagesFor(ctx, sessionID)
}

// MarkProcessed stamps the session dreamed so it is consumed exactly once.
func (s *StoreSource) MarkProcessed(ctx context.Context, sessionID string) error {
	return s.sessions.MarkDreamed(ctx, sessionID)
}
