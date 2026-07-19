package session

import (
	"context"
	"sync"
)

// StreamEvent is one live content frame for an active run — a text/thinking
// delta or a tool frame. It is the live-delivery counterpart to the durable
// message store: frames are transient, fanned out at full speed, and discarded
// once the run settles. Offset is monotonic within the session's live stream so
// a reconnecting consumer can re-read from where it left off.
type StreamEvent struct {
	Offset  int64
	RunID   string
	Kind    string // "text" | "thinking" | "tool_use" | "tool_result" | ...
	Payload []byte // encoded frame, the same shape the SSE emitter renders
}

// StreamBroker delivers live run content to attached clients with no database
// on the path. It is the live half of the delivery/persistence split (design
// redis-stream-live D1): durability for content is the assembled message in the
// MessageStore; the broker only has to be fast and reconnect-readable.
//
// The port carries Redis-Streams semantics (offset-based Publish/Read, run-end
// cleanup) so the in-memory and Redis implementations present identical
// consumer behaviour — single→multi-instance is a config swap, not a code
// change. Implementations must be safe for concurrent use.
type StreamBroker interface {
	// Publish appends a live frame for the session and returns its offset. It
	// must not block on a durable store; a slow consumer is dropped rather than
	// allowed to stall the run.
	Publish(ctx context.Context, sessionID string, ev StreamEvent) (int64, error)

	// Read returns the frames after `after` currently retained for the session,
	// oldest first. Used for reconnect catch-up. after=0 reads the whole buffer.
	// It returns only what is retained now (a settled run may have been cleaned
	// up, yielding an empty slice); it does not block for future frames.
	Read(ctx context.Context, sessionID string, after int64) ([]StreamEvent, error)

	// Subscribe returns a channel receiving frames published after subscription,
	// plus an unsubscribe func. Live follow; combined with Read for catch-up.
	// A slow consumer's full buffer drops frames (recoverable via Read), never
	// blocking the run.
	Subscribe(sessionID string, buffer int) (<-chan StreamEvent, func())

	// Settle marks the session's run finished and schedules the stream for
	// cleanup (buffer reset in memory, TTL in Redis). Called once when the run
	// reaches a terminal status; after it, Read may return nothing.
	Settle(ctx context.Context, sessionID string) error
}

// memBroker is the single-instance in-memory StreamBroker. Each session keeps a
// bounded ring of recent frames (for reconnect catch-up) plus a subscriber set
// (for live follow). No database: the run never waits on a write.
type memBroker struct {
	mu       sync.Mutex
	streams  map[string]*liveStream // sessionID -> ring + subs
	capacity int                    // max frames retained per session
}

// liveStream holds one session's retained frames and live subscribers.
type liveStream struct {
	next   int64          // next offset to assign (monotonic, survives ring eviction)
	frames []StreamEvent  // ring buffer, oldest first, len <= capacity
	subs   map[chan StreamEvent]struct{}
}

// NewMemBroker creates an in-memory StreamBroker retaining the last `capacity`
// frames per session for reconnect catch-up.
func NewMemBroker(capacity int) StreamBroker {
	if capacity <= 0 {
		capacity = 1024
	}
	return &memBroker{streams: map[string]*liveStream{}, capacity: capacity}
}

func (b *memBroker) Publish(_ context.Context, sessionID string, ev StreamEvent) (int64, error) {
	b.mu.Lock()
	ls := b.streams[sessionID]
	if ls == nil {
		ls = &liveStream{subs: map[chan StreamEvent]struct{}{}}
		b.streams[sessionID] = ls
	}
	ls.next++
	ev.Offset = ls.next
	ls.frames = append(ls.frames, ev)
	if len(ls.frames) > b.capacity {
		// Evict oldest; offsets keep increasing so Read(after) stays correct.
		ls.frames = ls.frames[len(ls.frames)-b.capacity:]
	}
	subs := make([]chan StreamEvent, 0, len(ls.subs))
	for ch := range ls.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // drop for slow consumers; they recover via Read
		}
	}
	return ev.Offset, nil
}

func (b *memBroker) Read(_ context.Context, sessionID string, after int64) ([]StreamEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ls := b.streams[sessionID]
	if ls == nil {
		return nil, nil
	}
	var out []StreamEvent
	for _, ev := range ls.frames {
		if ev.Offset > after {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (b *memBroker) Subscribe(sessionID string, buffer int) (<-chan StreamEvent, func()) {
	ch := make(chan StreamEvent, buffer)
	b.mu.Lock()
	ls := b.streams[sessionID]
	if ls == nil {
		ls = &liveStream{subs: map[chan StreamEvent]struct{}{}}
		b.streams[sessionID] = ls
	}
	ls.subs[ch] = struct{}{}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		if ls := b.streams[sessionID]; ls != nil {
			delete(ls.subs, ch)
		}
		b.mu.Unlock()
	}
	return ch, unsub
}

func (b *memBroker) Settle(_ context.Context, sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Drop retained frames (the run's content is durable in the message store by
	// now); keep the subscriber set so any still-attached client sees the run's
	// terminal frame delivered live. A fresh run starts a clean ring.
	if ls := b.streams[sessionID]; ls != nil {
		ls.frames = nil
	}
	return nil
}
