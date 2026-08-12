package session

import (
	"context"
	"sync"
	"time"
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

var _ MessageStore = (*MemMessageStore)(nil)
