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

	// MessagesAfter returns a session's messages with id > afterID, ordered by
	// seq — the tail beyond a watermark. The dreaming worker uses it to read only
	// the messages it has not yet consolidated (incremental model).
	MessagesAfter(ctx context.Context, sessionID string, afterID int64) ([]StoredMessage, error)
}

// StoredMessagesToProvider converts durable StoredMessages back into canonical
// provider messages for the loop, preserving full blocks (thinking+signature,
// tool_use, tool_result). The run registry uses it to rebuild a parked run's
// history on resume (capability-gap O2), where the transport's converter is
// out of reach (session must not depend on chatapi).
func StoredMessagesToProvider(stored []StoredMessage) []provider.Message {
	out := make([]provider.Message, 0, len(stored))
	for _, m := range stored {
		out = append(out, provider.Message{Role: m.Role, Content: m.Content})
	}
	return out
}
