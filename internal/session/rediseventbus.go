package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisEventBus is the multi-instance EventBus over Redis Pub/Sub. Lifecycle
// events (running/done/failed/cancelled/user) are published to a per-session
// channel so clients attached on any instance receive them live. Durability
// stays in the run event log (Store); Redis is only the cross-instance live
// pipe, matching memBus semantics: best-effort, drop on a slow consumer, and
// recover any gap via Replay.
type redisEventBus struct {
	cli *redis.Client
}

// NewRedisEventBus creates an EventBus over the Redis at addr.
func NewRedisEventBus(addr string) EventBus {
	return &redisEventBus{cli: redis.NewClient(&redis.Options{Addr: addr})}
}

// NewRedisEventBusFromClient builds a bus over an existing client (tests).
func NewRedisEventBusFromClient(cli *redis.Client) EventBus {
	return &redisEventBus{cli: cli}
}

func (b *redisEventBus) channel(sessionID string) string { return "session:" + sessionID + ":events" }

// Publish serializes the event and publishes it to the session's channel.
// Best-effort per the EventBus contract: an error is logged, not returned — the
// durable run log remains the source of truth and Replay fills any gap.
func (b *redisEventBus) Publish(sessionID string, e Event) {
	data, err := json.Marshal(e)
	if err != nil {
		slog.Warn("redis event bus: marshal event", "session", sessionID, "err", err)
		return
	}
	if err := b.cli.Publish(context.Background(), b.channel(sessionID), data).Err(); err != nil {
		slog.Warn("redis event bus: publish", "session", sessionID, "err", err)
	}
}

// Subscribe subscribes to the session's channel and forwards deserialized events
// to a buffered channel, dropping on a full buffer (slow consumer) like memBus.
// The returned unsubscribe cancels the forwarder and closes the Redis
// subscription; the Go channel is left open so an in-flight forward never panics
// on a closed channel (mirroring memBus).
//
// A transient Redis error must not kill the subscription: the forwarder backs
// off exponentially (1s → 30s cap, matching the MCP reconnect pattern) and
// rebuilds the Pub/Sub subscription before retrying. Events published during
// the gap are lost — Pub/Sub has no replay — which is the documented best-effort
// contract; the durable run log and Replay fill any gap. Only ctx cancellation
// is terminal.
func (b *redisEventBus) Subscribe(sessionID string, buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	ctx, cancel := context.WithCancel(context.Background())

	// current is the live Pub/Sub handle, replaced on every reconnect. The
	// mutex guards it so the unsubscribe path always closes the CURRENT handle,
	// never a stale one (a stale close would leak the replacement).
	var mu sync.Mutex
	current := b.cli.Subscribe(ctx, b.channel(sessionID))

	// reconnect closes the dead handle and subscribes fresh.
	reconnect := func() {
		mu.Lock()
		defer mu.Unlock()
		_ = current.Close()
		current = b.cli.Subscribe(ctx, b.channel(sessionID))
	}

	go func() {
		backoff := time.Second
		for {
			mu.Lock()
			ps := current
			mu.Unlock()
			msg, err := ps.ReceiveMessage(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return // cancelled
				}
				// Transient error: the subscription is dead; rebuild it and
				// back off before the next attempt.
				reconnect()
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			backoff = time.Second
			var e Event
			if err := json.Unmarshal([]byte(msg.Payload), &e); err != nil {
				continue
			}
			select {
			case ch <- e:
			default: // drop for slow consumers; they recover via Replay
			}
		}
	}()
	return ch, func() {
		cancel()
		mu.Lock()
		_ = current.Close()
		mu.Unlock()
	}
}
