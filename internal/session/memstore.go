package session

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStore is an in-memory Store for tests and early development.
type MemStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	runs     map[string]*Run   // runID
	bySess   map[string][]*Run // sessionID -> runs
	events   map[string][]Event // runID -> events
}

// NewMemStore creates an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		sessions: map[string]*Session{},
		runs:     map[string]*Run{},
		bySess:   map[string][]*Run{},
		events:   map[string][]Event{},
	}
}

func (m *MemStore) CreateSession(_ context.Context, userID, title string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Session{ID: uuid.NewString(), UserID: userID, Title: title, Status: SessionActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	m.sessions[s.ID] = s
	return *s, nil
}

func (m *MemStore) GetSession(_ context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, errors.New("session not found")
	}
	return *s, nil
}

func (m *MemStore) EndSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.Status = SessionEnded
	}
	return nil
}

func (m *MemStore) ListIdleSessions(_ context.Context, before time.Time) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.Status == SessionActive && s.UpdatedAt.Before(before) {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (m *MemStore) CreateRun(_ context.Context, sessionID string, seq int) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &Run{ID: uuid.NewString(), SessionID: sessionID, Seq: seq, Status: RunQueued, CreatedAt: time.Now()}
	m.runs[r.ID] = r
	m.bySess[sessionID] = append(m.bySess[sessionID], r)
	return *r, nil
}

func (m *MemStore) UpdateRunStatus(_ context.Context, runID string, status RunStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok {
		r.Status = status
	}
	return nil
}

func (m *MemStore) ActiveRun(_ context.Context, sessionID string) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.bySess[sessionID] {
		if r.Status.Active() {
			return *r, true, nil
		}
	}
	return Run{}, false, nil
}

func (m *MemStore) NextRunSeq(_ context.Context, sessionID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bySess[sessionID]) + 1, nil
}

func (m *MemStore) AppendEvent(_ context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.CreatedAt = time.Now()
	m.events[e.RunID] = append(m.events[e.RunID], e)
	return nil
}

func (m *MemStore) EventsAfter(_ context.Context, runID string, after int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events[runID] {
		if e.Offset > after {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out, nil
}
