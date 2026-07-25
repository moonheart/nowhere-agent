package session

import (
	"context"
	"encoding/json"
	"log/slog"

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
func (b *redisEventBus) Subscribe(sessionID string, buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	ctx, cancel := context.WithCancel(context.Background())
	pubsub := b.cli.Subscribe(ctx, b.channel(sessionID))
	go func() {
		for {
			msg, err := pubsub.ReceiveMessage(ctx)
			if err != nil {
				return // ctx cancelled or subscription closed
			}
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
		_ = pubsub.Close()
	}
}
