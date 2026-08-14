package chatapi

import (
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/session"
)

// waitFor polls cond until it holds or a deadline passes (the scheduler test
// helper's pattern). Tests used to sleep a fixed beat for async events
// (subscriptions, retained frames, run settlement); a poll loop is faster on
// fast machines and robust on slow ones. msg describes the condition on
// failure.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline: " + msg)
}

// countingBroker wraps a StreamBroker and counts current subscribers per
// session, so a test can wait for an attach to register its broker
// subscription instead of sleeping a fixed beat before releasing a run's gate.
type countingBroker struct {
	session.StreamBroker
	mu   sync.Mutex
	subs map[string]int
}

// newCountingBroker wraps inner with subscriber counting.
func newCountingBroker(inner session.StreamBroker) *countingBroker {
	return &countingBroker{StreamBroker: inner, subs: map[string]int{}}
}

func (b *countingBroker) Subscribe(sessionID string, buffer int) (<-chan session.StreamEvent, func()) {
	ch, unsub := b.StreamBroker.Subscribe(sessionID, buffer)
	b.mu.Lock()
	b.subs[sessionID]++
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if b.subs[sessionID] <= 1 {
			delete(b.subs, sessionID)
		} else {
			b.subs[sessionID]--
		}
		b.mu.Unlock()
		unsub()
	}
}

// subscribers reports how many live subscriptions the session currently has.
func (b *countingBroker) subscribers(sessionID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subs[sessionID]
}
