package session

import (
	"context"
	"encoding/json"
	"strings"
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
	// Usage is the token usage of the single LLM call that produced this
	// message (one assistant message == one LLM call). Nil on user/tool_result
	// rows, which are not LLM responses. Persisted as the messages.usage_* cols.
	Usage *provider.Usage
	// Metadata carries per-message extras beyond the block content (migration
	// 000042): a failed run's terminal error text is attached as {"error":
	// "..."} so a reloaded client can surface the failure and offer a retry.
	// Nil on rows without extras. Never fed back to the model on resume.
	Metadata json.RawMessage
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

	// MessagesPage returns up to limit messages with id > afterID, ordered by
	// seq — the keyset-paged form of MessagesFor for streaming consumers. The
	// cursor is the message id (ids ascend with seq, so paging by id preserves
	// seq order). Export dumps a user's whole conversation history; a page
	// bounds the working set instead of holding every message in memory at
	// once, mirroring how the SSE/history paths stream rather than load.
	MessagesPage(ctx context.Context, sessionID string, afterID int64, limit int) ([]StoredMessage, error)

	// MessagesTail returns up to limit messages with id < beforeID, ordered by
	// seq (ascending) — the newest messages older than the cursor, for the
	// history tail page. beforeID <= 0 reads the conversation's newest limit
	// messages. Ids ascend with seq, so the returned slice is the exact tail
	// the client renders, and the first message's id is the cursor for the
	// next (older) page. The bounded form of MessagesFor for long sessions: a
	// client that renders only the tail pays for `limit` rows, not the whole
	// conversation.
	MessagesTail(ctx context.Context, sessionID string, beforeID int64, limit int) ([]StoredMessage, error)

	// LastAssistantText returns the trimmed text of the most recent assistant
	// message whose content carries text, scanning back at most limit assistant
	// messages (newest first). It is the cheap bounded form of MessagesFor for
	// one-shot summaries (webhook payloads): a conversation may hold thousands
	// of rows, but the last answer lives near the tail, so a full load would be
	// wasted work. Empty string when no such message exists within the bound.
	LastAssistantText(ctx context.Context, sessionID string, limit int) (string, error)

	// LastAssistantMessage returns the run's most recent assistant message
	// (newest by seq), or nil when the run has none. It is the cheap bounded
	// form of MessagesFor for the failed-run error attach (attachRunError only
	// needs the last assistant message of the failing run, never the whole
	// conversation). A run is a single turn, so the query returns at most one
	// row regardless of session length.
	LastAssistantMessage(ctx context.Context, sessionID, runID string) (*StoredMessage, error)

	// SetMessageMetadata replaces one message's metadata JSON (used to attach a
	// failed run's terminal error to its last assistant message after the run
	// settles — the message row is append-only, so the update is the seam).
	// Best-effort by design: the block content is unaffected and a failure is
	// only logged by the caller.
	SetMessageMetadata(ctx context.Context, id int64, metadata json.RawMessage) error
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

// assistantText concatenates a message's text blocks in content order,
// trimmed. Empty when the message carries no text content (e.g. a tool-only
// round). Shared by the LastAssistantText implementations of both stores so
// the notion of "the message's text" cannot drift between them.
func assistantText(blocks []provider.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == provider.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
