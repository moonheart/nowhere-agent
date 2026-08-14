package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Manager owns sandbox lifecycle per session (design D3/D13): create on
// demand, deferred stop after session-end, and scheduled destroy. The actual
// timing/scheduled reaping is driven by the unified scheduler (D15); Manager
// tracks state and applies the deferred-stop policy.
type Manager struct {
	port Port

	mu     sync.Mutex
	active map[string]*entry // sessionID -> entry
}

type entry struct {
	handle   Handle
	state    State
	lastUsed time.Time
	stopAt   time.Time
}

// State is the lifecycle state of a session's sandbox.
type State string

const (
	StateRunning State = "running"
	// StateStopped is the deferred-stop grace period: container paused but
	// resumable; workspace already solidified.
	StateStopped   State = "stopped"
	StateDestroyed State = "destroyed"
)

// NewManager creates a Manager over a Port.
func NewManager(port Port) *Manager {
	return &Manager{port: port, active: map[string]*entry{}}
}

// Ensure returns a running sandbox for the session, creating one if needed.
func (m *Manager) Ensure(ctx context.Context, sessionID string, opts Options) (Handle, error) {
	m.mu.Lock()
	e, ok := m.active[sessionID]
	if ok && e.state == StateRunning {
		e.lastUsed = time.Now()
		h := e.handle
		m.mu.Unlock()
		return h, nil
	}
	m.mu.Unlock()

	// A session resuming inside the deferred-stop grace period still holds a
	// stopped handle whose underlying resource — e.g. a fixed-name Docker
	// container — would collide with the fresh Create. Best-effort destroy it
	// first, so the recreate path works and the sweep never has to catch up:
	// a failed destroy leaves the old resource in place, and Create surfaces
	// its own conflict error rather than silently degrading the run.
	if ok {
		_ = m.port.Destroy(ctx, e.handle)
	}

	h, err := m.port.Create(ctx, sessionID, opts)
	if err != nil {
		return Handle{}, fmt.Errorf("create sandbox: %w", err)
	}
	m.mu.Lock()
	m.active[sessionID] = &entry{handle: h, state: StateRunning, lastUsed: time.Now()}
	m.mu.Unlock()
	return h, nil
}

// Get returns the running sandbox for a session, or false.
func (m *Manager) Get(sessionID string) (Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.active[sessionID]; ok && e.state == StateRunning {
		return e.handle, true
	}
	return Handle{}, false
}

// MarkSessionEnded begins the deferred-stop grace period: the sandbox stays
// resumable until now+delay, after which it becomes eligible for destroy.
func (m *Manager) MarkSessionEnded(sessionID string, delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.active[sessionID]; ok && e.state == StateRunning {
		e.state = StateStopped
		e.stopAt = time.Now().Add(delay)
	}
}

// Sweep destroys sandboxes whose deferred-stop deadline has passed. Called by
// the scheduler. Returns the sessionIDs destroyed.
func (m *Manager) Sweep(ctx context.Context, now time.Time) ([]string, error) {
	var doomed []string
	m.mu.Lock()
	for id, e := range m.active {
		if e.state == StateStopped && !now.Before(e.stopAt) {
			doomed = append(doomed, id)
		}
	}
	m.mu.Unlock()

	var destroyed []string
	var errs []error
	for _, id := range doomed {
		m.mu.Lock()
		e := m.active[id]
		m.mu.Unlock()
		if e == nil {
			continue
		}
		if err := m.port.Destroy(ctx, e.handle); err != nil {
			// Collect and keep sweeping: one failing sandbox must not leave the
			// rest of the batch to rot until the next sweep.
			errs = append(errs, fmt.Errorf("destroy %s: %w", id, err))
			continue
		}
		m.mu.Lock()
		e.state = StateDestroyed
		delete(m.active, id)
		m.mu.Unlock()
		destroyed = append(destroyed, id)
	}
	if len(errs) > 0 {
		return destroyed, errors.Join(errs...)
	}
	return destroyed, nil
}

// StateOf returns the lifecycle state for a session's sandbox.
func (m *Manager) StateOf(sessionID string) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.active[sessionID]; ok {
		return e.state
	}
	return StateDestroyed
}
