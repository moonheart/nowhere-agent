package session

import (
	"context"
	"time"

	"nowhere-agent/internal/provider"
)

// StoredMessage is one conversation message persisted in full-fidelity block
// form (design: persist-raw-messages). It is the authoritative record of a
// single turn in the conversation — distinct from Event, which records a
// streaming/lifecycle frame for attach and replay. Content holds the canonical
// []provider.Block (text, thinking incl. signature, tool_use, tool_result;
// image blocks carry a workspace-relative path pointer, never the payload).
type StoredMessage struct {
	ID        int64
	SessionID string
	RunID     string
	Seq       int
	Role      provider.Role
	Content   []provider.Block
	CreatedAt time.Time
}

// MessageStore persists conversation messages in original block form and reads
// them back as the authoritative conversation history. It is a separate port
// from Store (the run/event log) so the two can evolve independently; both are
// written on the same run path. Implementations must be safe for concurrent use.
type MessageStore interface {
	// AppendMessage persists one message, assigning the next per-session seq.
	// Implementations continue the sequence from the durable max so appends
	// stay monotonic even after a run settles mid-stream.
	AppendMessage(ctx context.Context, msg StoredMessage) (StoredMessage, error)

	// MessagesFor returns a session's messages ordered by seq (the full
	// conversation across all runs), for authoritative history rebuild.
	MessagesFor(ctx context.Context, sessionID string) ([]StoredMessage, error)
}
