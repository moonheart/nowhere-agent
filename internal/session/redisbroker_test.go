package session

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedisBroker spins up an in-memory Redis and a broker over it.
func newTestRedisBroker(t *testing.T) (StreamBroker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	return NewRedisBrokerFromClient(cli, 100, 50*time.Millisecond), mr
}

func TestRedisBrokerPublishReadRoundTrip(t *testing.T) {
	b, _ := newTestRedisBroker(t)
	ctx := context.Background()

	for i, kind := range []string{"text", "text", "thinking"} {
		if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: kind, Payload: []byte{byte('a' + i)}}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	got, err := b.Read(ctx, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Read(0) = %d frames, want 3", len(got))
	}
	if got[0].Kind != "text" || got[2].Kind != "thinking" {
		t.Errorf("kinds = %q,%q,%q", got[0].Kind, got[1].Kind, got[2].Kind)
	}
	if string(got[1].Payload) != "b" {
		t.Errorf("payload[1] = %q want b", got[1].Payload)
	}
	if got[0].RunID != "r1" {
		t.Errorf("run id = %q", got[0].RunID)
	}
	// Offsets increase monotonically.
	for i := 1; i < len(got); i++ {
		if got[i].Offset <= got[i-1].Offset {
			t.Errorf("offsets not increasing: %d then %d", got[i-1].Offset, got[i].Offset)
		}
	}
}

func TestRedisBrokerReadAfterSkipsSeen(t *testing.T) {
	b, _ := newTestRedisBroker(t)
	ctx := context.Background()
	var offs []int64
	for i := 0; i < 4; i++ {
		off, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte{byte('0' + i)}})
		if err != nil {
			t.Fatal(err)
		}
		offs = append(offs, off)
	}

	// Reconnect from the second offset: only frames after it come back.
	got, err := b.Read(ctx, "s1", offs[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Read(after=2nd) = %d frames, want 2", len(got))
	}
	if string(got[0].Payload) != "2" || string(got[1].Payload) != "3" {
		t.Errorf("reconnect frames = %q,%q want 2,3", got[0].Payload, got[1].Payload)
	}
}

func TestRedisBrokerSubscribeReceivesLiveFrames(t *testing.T) {
	b, _ := newTestRedisBroker(t)
	ctx := context.Background()
	ch, unsub := b.Subscribe("s1", 8)
	defer unsub()

	// Let the poller resolve the stream tail and enter its blocking read before
	// we publish, so the live frame is observed (not skipped as pre-existing).
	time.Sleep(50 * time.Millisecond)
	if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte("live")}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if string(ev.Payload) != "live" {
			t.Errorf("live payload = %q", ev.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive live frame")
	}
}

// TestRedisBrokerSlowConsumerRecoversDroppedFrames pins the pollLoop fix: a
// full channel must not advance the poller's read position, so frames dropped
// for a slow consumer are re-read once it drains — live-follow recovers without
// a reload, and every frame arrives exactly once, in order.
func TestRedisBrokerSlowConsumerRecoversDroppedFrames(t *testing.T) {
	b, _ := newTestRedisBroker(t)
	ctx := context.Background()
	// Buffer 1: publishes outrun the consumer, forcing drops.
	ch, unsub := b.Subscribe("s1", 1)
	defer unsub()

	// Let the poller resolve the stream tail and enter its blocking read.
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte{byte('0' + i)}}); err != nil {
			t.Fatal(err)
		}
	}

	// Drain continuously: every frame must arrive — a dropped frame stalls the
	// poller's position until the channel drains, then is re-read (XREAD is
	// strictly greater than the last delivered ID, so nothing duplicates).
	got := ""
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < 5 && time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			got += string(ev.Payload)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if got != "01234" {
		t.Fatalf("frames received = %q, want all five in order (no drops, no dups)", got)
	}
}

// TestRedisBrokerSlowConsumerNoBusyLoop pins the full-drop backoff: when a
// slow consumer's channel drops an ENTIRE batch, the poller must pause instead
// of re-reading the same entries in a tight loop. The XREAD count of the
// broker's poll cycle is small, so this drives enough entries to force a
// whole-batch drop and asserts the poller stops hammering (XREAD call volume
// stays bounded once the backoff kicks in).
func TestRedisBrokerSlowConsumerNoBusyLoop(t *testing.T) {
	b, mr := newTestRedisBroker(t)
	ctx := context.Background()
	ch, unsub := b.Subscribe("s1", 2)
	defer unsub()

	// Let the poller resolve the stream tail and enter its blocking read.
	time.Sleep(50 * time.Millisecond)
	// Far more entries than the channel buffer: every poll cycle drops its
	// whole batch (nobody drains), so without the backoff the poller would
	// spin at full speed.
	for i := 0; i < 500; i++ {
		if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte{byte('0' + i%10)}}); err != nil {
			t.Fatal(err)
		}
	}

	// Measure XREAD volume over a window: with the 50ms backoff a busy loop
	// would issue far more reads than one per ~50ms.
	before := mr.CommandCount()
	time.Sleep(400 * time.Millisecond)
	after := mr.CommandCount()
	reads := after - before
	// Budget: 400ms / 50ms = 8 cycles, plus slop for the poller's initial
	// burst before the backoff engages.
	if reads > 200 {
		t.Errorf("XREADs in 400ms = %d, want bounded by the full-drop backoff (busy loop)", reads)
	}
	_ = ch
}

// TestRedisBrokerReadLoopsPastSingleBatchCap: a catch-up reading more frames
// than one XREAD batch (Count 1024) returns them ALL, in order — the old
// single-read would lose the middle of the retained buffer once the run
// settles and the stream expires.
func TestRedisBrokerReadLoopsPastSingleBatchCap(t *testing.T) {
	mr := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cli.Close()
	b := NewRedisBrokerFromClient(cli, 5000, time.Minute)
	ctx := context.Background()

	// 2500 frames: 3 XREAD batches of 1024/1024/452.
	for i := 0; i < 2500; i++ {
		if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte{byte('0' + i%10)}}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := b.Read(ctx, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2500 {
		t.Fatalf("Read = %d frames, want all 2500 (past the 1024 single-batch cap)", len(got))
	}
	for i, e := range got {
		if string(e.Payload) != string([]byte{byte('0' + i%10)}) {
			t.Fatalf("frame %d payload = %q, want %q (order must hold across batches)", i, e.Payload, byte('0'+i%10))
		}
		if i > 0 && e.Offset <= got[i-1].Offset {
			t.Fatalf("offsets not increasing at %d", i)
		}
	}
}

func TestRedisBrokerSettleAppliesTTL(t *testing.T) {
	b, mr := newTestRedisBroker(t)
	ctx := context.Background()
	if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	ttl := mr.TTL("session:s1:stream")
	if ttl <= 0 {
		t.Errorf("stream TTL after Settle = %v, want > 0 (expires after settle)", ttl)
	}
}

func TestRedisBrokerStreamCappedByMaxLen(t *testing.T) {
	mr := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cli.Close()
	b := NewRedisBrokerFromClient(cli, 10, time.Minute)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte{byte('0' + i%10)}}); err != nil {
			t.Fatal(err)
		}
	}
	// Approximate trim keeps the stream near maxLen, not 50.
	length, err := cli.XLen(ctx, "session:s1:stream").Result()
	if err != nil {
		t.Fatal(err)
	}
	if length > 20 {
		t.Errorf("stream length = %d, want bounded near maxLen 10", length)
	}
}

func TestRedisBrokerSessionsIsolated(t *testing.T) {
	b, _ := newTestRedisBroker(t)
	ctx := context.Background()
	_, _ = b.Publish(ctx, "a", StreamEvent{RunID: "r", Kind: "text", Payload: []byte("A")})
	_, _ = b.Publish(ctx, "b", StreamEvent{RunID: "r", Kind: "text", Payload: []byte("B")})
	ga, _ := b.Read(ctx, "a", 0)
	gb, _ := b.Read(ctx, "b", 0)
	if len(ga) != 1 || string(ga[0].Payload) != "A" {
		t.Errorf("session a = %+v", ga)
	}
	if len(gb) != 1 || string(gb[0].Payload) != "B" {
		t.Errorf("session b = %+v", gb)
	}
}

// TestRedisBrokerSubscriberSurvivesTransientError pins the pollLoop fix: a
// transient Redis error (simulated with miniredis.SetError) must NOT kill the
// subscription — after the error clears, the poller resumes from its last
// delivered position and the live stream keeps flowing.
func TestRedisBrokerSubscriberSurvivesTransientError(t *testing.T) {
	b, mr := newTestRedisBroker(t)
	ctx := context.Background()
	ch, unsub := b.Subscribe("s1", 8)
	defer unsub()

	// Baseline: a frame flows before the outage.
	time.Sleep(50 * time.Millisecond)
	if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte("before")}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if string(ev.Payload) != "before" {
			t.Errorf("baseline payload = %q", ev.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("baseline frame not delivered")
	}

	// Simulate an outage: every command fails. The poller must not exit.
	mr.SetError("ERR simulated outage")
	time.Sleep(150 * time.Millisecond)
	mr.SetError("")

	// After recovery, a new frame must still arrive — the subscriber survived.
	if _, err := b.Publish(ctx, "s1", StreamEvent{RunID: "r1", Kind: "text", Payload: []byte("after")}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if string(ev.Payload) != "after" {
			t.Errorf("post-recovery payload = %q, want the frame published after the outage", ev.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber died on the transient error: no frame after recovery")
	}
}

func TestStreamIDOffsetMappingMonotonic(t *testing.T) {
	// The offset mapping must be monotonic across seq and ms boundaries so
	// Read(after) ordering is correct.
	ids := []string{"100-0", "100-1", "101-0", "250-3"}
	prev := int64(-1)
	for _, id := range ids {
		off := streamIDToOffset(id)
		if off <= prev {
			t.Errorf("offset for %s = %d not > %d", id, off, prev)
		}
		prev = off
	}
	// Round-trip through offsetToStreamID is the identity for these ids.
	for _, id := range ids {
		if got := offsetToStreamID(streamIDToOffset(id)); got != id {
			t.Errorf("round-trip %s -> %s", id, got)
		}
	}
}

// TestStreamIDOffsetNoMillisecondCollision pins the seq scaling: a burst of
// more than 1000 entries within one millisecond (seq >= 1000, which Redis
// assigns when a stream outpaces the clock) must not collide with the next
// millisecond's first entry.
func TestStreamIDOffsetNoMillisecondCollision(t *testing.T) {
	a := streamIDToOffset("100-999")    // last entry of ms 100 under the old scale
	b := streamIDToOffset("100-1000")   // 1000+ entries in one millisecond
	d := streamIDToOffset("100-100000") // extreme burst — still below the 1e6 scale
	c := streamIDToOffset("101-0")      // first entry of the next millisecond
	if !(a < b && b < d && d < c) {
		t.Fatalf("offsets must be strictly increasing, got %d, %d, %d, %d", a, b, d, c)
	}
	// Round-trip stays the identity across the boundary.
	for _, id := range []string{"100-999", "100-1000", "100-100000", "101-0"} {
		if got := offsetToStreamID(streamIDToOffset(id)); got != id {
			t.Errorf("round-trip %s -> %s", id, got)
		}
	}
}
