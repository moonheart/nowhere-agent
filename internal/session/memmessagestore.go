package session

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"nowhere-agent/internal/provider"
)

// MemMessageStore is an in-memory MessageStore for tests and dev. It assigns
// per-session seq from the length of the session's slice, which is monotonic
// because appends only ever add (no deletes).
type MemMessageStore struct {
	mu     sync.Mutex
	next   int64
	bySess map[string][]StoredMessage
}

// NewMemMessageStore creates an empty in-memory MessageStore.
func NewMemMessageStore() *MemMessageStore {
	return &MemMessageStore{bySess: map[string][]StoredMessage{}}
}

// AppendMessage assigns the next per-session seq and appends. A non-zero
// msg.ID is a pre-provisioned id (a run_steps intent reserved it; mem uses
// negative ids so they can never collide with the positive auto-assigned
// ones) and is honored verbatim.
func (m *MemMessageStore) AppendMessage(_ context.Context, msg StoredMessage) (StoredMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.ID == 0 {
		m.next++
		msg.ID = m.next
	}
	msg.Seq = len(m.bySess[msg.SessionID])
	msg.CreatedAt = time.Now()
	m.bySess[msg.SessionID] = append(m.bySess[msg.SessionID], msg)
	return msg, nil
}

// MessagesFor returns the session's messages in seq order.
func (m *MemMessageStore) MessagesFor(_ context.Context, sessionID string) ([]StoredMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.bySess[sessionID]
	out := make([]StoredMessage, len(src))
	copy(out, src)
	return out, nil
}

// MessagesAfter returns the session's messages with id > afterID, in seq order.
func (m *MemMessageStore) MessagesAfter(_ context.Context, sessionID string, afterID int64) ([]StoredMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []StoredMessage
	for _, msg := range m.bySess[sessionID] {
		if msg.ID > afterID {
			out = append(out, msg)
		}
	}
	return out, nil
}

// MessagesPage returns up to limit messages with id > afterID, in seq order
// (see MessageStore.MessagesPage).
func (m *MemMessageStore) MessagesPage(_ context.Context, sessionID string, afterID int64, limit int) ([]StoredMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []StoredMessage
	for _, msg := range m.bySess[sessionID] {
		if msg.ID > afterID {
			out = append(out, msg)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// MessagesTail returns up to limit messages with id < beforeID, in seq order
// (see MessageStore.MessagesTail): the newest messages older than the cursor.
func (m *MemMessageStore) MessagesTail(_ context.Context, sessionID string, beforeID int64, limit int) ([]StoredMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		return nil, nil
	}
	src := m.bySess[sessionID]
	var out []StoredMessage
	for i := len(src) - 1; i >= 0 && len(out) < limit; i-- {
		if beforeID > 0 && src[i].ID >= beforeID {
			continue
		}
		out = append(out, src[i])
	}
	slices.Reverse(out)
	return out, nil
}

// SetMessageMetadata replaces one message's metadata JSON, located by id. A
// missing id is not an error (best-effort contract); the slice is copied so
// the stored row is updated in place.
func (m *MemMessageStore) SetMessageMetadata(_ context.Context, id int64, metadata json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sess, msgs := range m.bySess {
		for i := range msgs {
			if msgs[i].ID == id {
				msgs[i].Metadata = append(json.RawMessage(nil), metadata...)
				m.bySess[sess] = msgs
				return nil
			}
		}
	}
	return nil
}

// LastAssistantText returns the most recent assistant text (see
// MessageStore.LastAssistantText), scanning the session's tail backwards.
func (m *MemMessageStore) LastAssistantText(_ context.Context, sessionID string, limit int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		return "", nil
	}
	src := m.bySess[sessionID]
	for i := len(src) - 1; i >= 0 && limit > 0; i-- {
		if src[i].Role != provider.RoleAssistant {
			continue
		}
		limit--
		if s := assistantText(src[i].Content); s != "" {
			return s, nil
		}
	}
	return "", nil
}

// LastAssistantMessage returns the run's most recent assistant message (see
// MessageStore.LastAssistantMessage), scanning the session's tail backwards.
func (m *MemMessageStore) LastAssistantMessage(_ context.Context, sessionID, runID string) (*StoredMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.bySess[sessionID]
	for i := len(src) - 1; i >= 0; i-- {
		if src[i].Role == provider.RoleAssistant && src[i].RunID == runID {
			msg := src[i]
			return &msg, nil
		}
	}
	return nil, nil
}

var _ MessageStore = (*MemMessageStore)(nil)
