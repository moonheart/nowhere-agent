package session

import "sync"

// EventBus fans run events out to attached clients. It is a live-delivery layer
// only: the durable run log (Store) is the single source of truth, and a slow or
// disconnected subscriber recovers via Replay. The bus is a port so a
// Redis-backed implementation can fan out across instances without the session
// or transport layers changing (design decouple-run-ownership D2).
type EventBus interface {
	// Publish delivers an event to every current subscriber of the session.
	// Delivery is best-effort: a slow consumer's full buffer drops the event
	// rather than block the run, because Replay is the gap-filler.
	Publish(sessionID string, e Event)
	// Subscribe returns a channel receiving events published after subscription,
	// plus an unsubscribe func that closes the channel.
	Subscribe(sessionID string, buffer int) (<-chan Event, func())
}

// memBus is the single-instance in-memory EventBus.
type memBus struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{} // sessionID -> subscriber channels
}

// NewMemBus creates an in-memory EventBus.
func NewMemBus() EventBus {
	return &memBus{subs: map[string]map[chan Event]struct{}{}}
}

// Publish fans the event out to each subscriber, dropping to a full buffer so a
// slow consumer never blocks the run (Replay recovers the gap).
func (b *memBus) Publish(sessionID string, e Event) {
	b.mu.Lock()
	subs := b.subs[sessionID]
	b.mu.Unlock()
	for ch := range subs {
		select {
		case ch <- e:
		default: // drop for slow consumers rather than block the run
		}
	}
}

// Subscribe registers a buffered channel for the session's events.
//
// The returned unsubscribe removes the channel from the subscriber set but does
// NOT close it: a concurrent Publish may have already snapshotted the channel
// and be about to send, and sending on a closed channel panics. Leaving the
// channel open lets an in-flight send land harmlessly in the buffer; the channel
// is garbage-collected once the subscriber drops its reference. Subscribers
// terminate on the run's terminal event or their own context, not on close.
func (b *memBus) Subscribe(sessionID string, buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[chan Event]struct{}{}
	}
	b.subs[sessionID][ch] = struct{}{}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		delete(b.subs[sessionID], ch)
		b.mu.Unlock()
	}
	return ch, unsub
}
