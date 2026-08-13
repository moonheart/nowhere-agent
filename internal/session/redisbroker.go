package session

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisBroker is the multi-instance StreamBroker over Redis Streams. Each
// session's live content lives in one stream key (`session:{id}:stream`):
// publish is XADD, consume is XREAD, and Settle applies a short TTL so the
// transient stream self-destructs once the run's live purpose ends. Durable
// content stays in Postgres; Redis is only the cross-instance live pipe.
//
// Stream entry IDs are the broker offsets ("<seq>-0" with a per-session
// monotonic seq we let Redis assign via "*"), so a consumer on any instance
// reads the same ordered stream.
type redisBroker struct {
	cli    *redis.Client
	maxLen int64         // approximate stream cap (MAXLEN ~)
	ttl    time.Duration // applied at Settle
	poll   time.Duration // XREAD BLOCK timeout per read cycle
}

// NewRedisBroker creates a StreamBroker over the Redis at addr. maxLen bounds
// stream growth (approximate trim); ttl is how long a settled run's stream
// lingers for reconnect before expiring.
func NewRedisBroker(addr string, maxLen int64, ttl time.Duration) StreamBroker {
	if maxLen <= 0 {
		maxLen = 4096
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &redisBroker{
		cli:    redis.NewClient(&redis.Options{Addr: addr}),
		maxLen: maxLen,
		ttl:    ttl,
		poll:   2 * time.Second,
	}
}

// NewRedisBrokerFromClient builds a broker over an existing client (tests).
func NewRedisBrokerFromClient(cli *redis.Client, maxLen int64, ttl time.Duration) StreamBroker {
	if maxLen <= 0 {
		maxLen = 4096
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &redisBroker{cli: cli, maxLen: maxLen, ttl: ttl, poll: 50 * time.Millisecond}
}

// PingRedis checks the Redis at addr is reachable, for fail-fast startup when
// the redis broker is selected.
func PingRedis(ctx context.Context, addr string) error {
	cli := redis.NewClient(&redis.Options{Addr: addr})
	defer cli.Close()
	return cli.Ping(ctx).Err()
}

func (b *redisBroker) key(sessionID string) string { return "session:" + sessionID + ":stream" }

func (b *redisBroker) Publish(ctx context.Context, sessionID string, ev StreamEvent) (int64, error) {
	id, err := b.cli.XAdd(ctx, &redis.XAddArgs{
		Stream: b.key(sessionID),
		MaxLen: b.maxLen,
		Approx: true,
		Values: map[string]any{
			"run":     ev.RunID,
			"kind":    ev.Kind,
			"payload": ev.Payload,
		},
	}).Result()
	if err != nil {
		return 0, err
	}
	return streamIDToOffset(id), nil
}

func (b *redisBroker) Read(ctx context.Context, sessionID string, after int64) ([]StreamEvent, error) {
	res, err := b.cli.XRead(ctx, &redis.XReadArgs{
		Streams: []string{b.key(sessionID), offsetToStreamID(after)},
		Count:   1024,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	if len(res) == 0 {
		return nil, nil
	}
	out := make([]StreamEvent, 0, len(res[0].Messages))
	for _, m := range res[0].Messages {
		out = append(out, messageToStreamEvent(m))
	}
	return out, nil
}

// Subscribe polls XREAD BLOCK in a loop and forwards entries to a channel. The
// returned unsubscribe cancels the poller; the channel is left open (an
// in-flight forward must not panic on a closed channel) and GC'd once dropped.
func (b *redisBroker) Subscribe(sessionID string, buffer int) (<-chan StreamEvent, func()) {
	ch := make(chan StreamEvent, buffer)
	ctx, cancel := context.WithCancel(context.Background())
	go b.pollLoop(ctx, sessionID, ch)
	return ch, func() { cancel() }
}

// pollLoop follows the stream, forwarding new entries until cancelled. It
// starts from the stream's current tail (equivalent to "$", but resolved
// eagerly so an entry published during startup isn't missed), then blocks on
// XREAD for subsequent entries. Catch-up of pre-existing entries is Read's job.
//
// A slow consumer's full channel drops entries, and the read position (`last`)
// is advanced ONLY on a successful delivery: the dropped entry is then re-read
// by the next XREAD cycle (XREAD returns entries strictly after `last`, so
// already-delivered ones are never duplicated) and forwarded once the channel
// drains. This is what keeps a dropped live frame recoverable — both via the
// retry here and via Read, which serves the same retained stream.
//
// A transient Redis error must not kill the subscription: the stream survives
// (it is retained until Settle's TTL), so the poller backs off exponentially
// (1s → 30s cap, matching the MCP reconnect pattern) and retries from the same
// `last` position — only ctx cancellation is terminal.
func (b *redisBroker) pollLoop(ctx context.Context, sessionID string, ch chan<- StreamEvent) {
	last := b.tailID(ctx, sessionID)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := b.cli.XRead(ctx, &redis.XReadArgs{
			Streams: []string{b.key(sessionID), last},
			Count:   256,
			Block:   b.poll,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) || ctx.Err() != nil {
				continue // timeout, no new entries — poll again
			}
			// Transient error (Redis restart, network blip): back off and retry
			// from the same position rather than dying — a subscriber that
			// returns on a hiccup freezes the client's live stream until the
			// run settles.
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
		for _, stream := range res {
			for _, m := range stream.Messages {
				ev := messageToStreamEvent(m)
				select {
				case ch <- ev:
					// The entry is delivered: advance the read position past it.
					// XREAD is strictly-greater, so a later cycle starting from
					// this ID re-reads only entries published after it.
					last = m.ID
				default: // drop for slow consumers; they recover via Read and
					// the next cycle's re-read (last is not advanced)
				}
			}
		}
	}
}

// tailID returns the stream's newest entry ID, or "0" if the stream is empty.
// Resolving it eagerly (rather than using "$") closes the race where an entry
// published before the first blocking XREAD would be skipped.
func (b *redisBroker) tailID(ctx context.Context, sessionID string) string {
	res, err := b.cli.XRevRangeN(ctx, b.key(sessionID), "+", "-", 1).Result()
	if err != nil || len(res) == 0 {
		return "0"
	}
	return res[0].ID
}

func (b *redisBroker) Settle(ctx context.Context, sessionID string) error {
	// Apply a short TTL rather than deleting immediately: a client reconnecting
	// right at settle can still catch the tail from the stream before it expires,
	// after which content comes from the message store.
	return b.cli.Expire(ctx, b.key(sessionID), b.ttl).Err()
}

// streamIDToOffset converts a Redis stream ID ("<ms>-<seq>") to a monotonic
// int64 offset. We use the millisecond+sequence composite; for our per-session
// streams the first component dominates ordering. seq is scaled by 1e6 (not
// 1000): a burst of >= 1000 entries in one millisecond would otherwise
// collide with the next millisecond's "ms+1-0" offset and Read(after) would
// skip or replay entries.
func streamIDToOffset(id string) int64 {
	ms, seq, _ := splitStreamID(id)
	return ms*1_000_000 + seq
}

func offsetToStreamID(off int64) string {
	if off <= 0 {
		return "0"
	}
	return strconv.FormatInt(off/1_000_000, 10) + "-" + strconv.FormatInt(off%1_000_000, 10)
}

func splitStreamID(id string) (ms, seq int64, err error) {
	var a, b string
	for i := 0; i < len(id); i++ {
		if id[i] == '-' {
			a, b = id[:i], id[i+1:]
			break
		}
	}
	if a == "" {
		ms, err = strconv.ParseInt(id, 10, 64)
		return ms, 0, err
	}
	ms, err = strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	seq, err = strconv.ParseInt(b, 10, 64)
	return ms, seq, err
}

func messageToStreamEvent(m redis.XMessage) StreamEvent {
	ev := StreamEvent{Offset: streamIDToOffset(m.ID)}
	if v, ok := m.Values["run"].(string); ok {
		ev.RunID = v
	}
	if v, ok := m.Values["kind"].(string); ok {
		ev.Kind = v
	}
	switch v := m.Values["payload"].(type) {
	case string:
		ev.Payload = []byte(v)
	case []byte:
		ev.Payload = v
	}
	return ev
}
