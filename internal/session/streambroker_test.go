package session

import (
	"context"
	"fmt"
	"testing"
)

func TestMemBrokerPublishAssignsMonotonicOffsets(t *testing.T) {
	b := NewMemBroker(16)
	ctx := context.Background()
	var last int64
	for i := 1; i <= 3; i++ {
		off, err := b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte(fmt.Sprintf("%d", i))})
		if err != nil {
			t.Fatal(err)
		}
		if off <= last {
			t.Errorf("offset %d not increasing after %d", off, last)
		}
		last = off
	}
	if last != 3 {
		t.Errorf("expected 3 offsets, last=%d", last)
	}
}

func TestMemBrokerReadAfterReturnsNewerFrames(t *testing.T) {
	b := NewMemBroker(16)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		_, _ = b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte{byte('0' + i)}})
	}

	all, _ := b.Read(ctx, "s1", 0)
	if len(all) != 5 {
		t.Fatalf("Read(0) got %d frames, want 5", len(all))
	}
	tail, _ := b.Read(ctx, "s1", 3)
	if len(tail) != 2 || tail[0].Offset != 4 || tail[1].Offset != 5 {
		t.Fatalf("Read(3) = %+v, want offsets 4,5", tail)
	}
	// Reconnect exactly at the tip yields nothing new.
	none, _ := b.Read(ctx, "s1", 5)
	if len(none) != 0 {
		t.Fatalf("Read(5) = %v, want empty", none)
	}
}

func TestMemBrokerReadEmptyForUnknownSession(t *testing.T) {
	b := NewMemBroker(8)
	got, err := b.Read(context.Background(), "nope", 0)
	if err != nil || got != nil {
		t.Errorf("unknown session: got=%v err=%v, want nil,nil", got, err)
	}
}

func TestMemBrokerRingEvictsOldestButOffsetsContinue(t *testing.T) {
	b := NewMemBroker(4)
	ctx := context.Background()
	for i := 1; i <= 10; i++ {
		_, _ = b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte{byte('0' + i%10)}})
	}
	// Only the last 4 are retained, with offsets 7..10.
	got, _ := b.Read(ctx, "s1", 0)
	if len(got) != 4 || got[0].Offset != 7 || got[3].Offset != 10 {
		t.Fatalf("ring = %+v, want offsets 7..10", got)
	}
	// A reconnect from an evicted offset still gets everything retained.
	reconn, _ := b.Read(ctx, "s1", 2)
	if len(reconn) != 4 {
		t.Fatalf("Read(2) after eviction = %d frames, want 4", len(reconn))
	}
}

func TestMemBrokerSubscribeReceivesLiveFrames(t *testing.T) {
	b := NewMemBroker(8)
	ctx := context.Background()
	ch, unsub := b.Subscribe("s1", 8)
	defer unsub()

	if _, err := b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if string(ev.Payload) != "x" {
			t.Errorf("live payload = %q", ev.Payload)
		}
	default:
		t.Error("subscriber did not receive live frame")
	}
}

func TestMemBrokerSlowConsumerDropsButPublishDoesNotBlock(t *testing.T) {
	b := NewMemBroker(8)
	ctx := context.Background()
	// Subscribe with a 1-frame buffer and never drain it.
	_, unsub := b.Subscribe("s1", 1)
	defer unsub()

	// Publishing far more than the buffer must not block.
	for i := 0; i < 100; i++ {
		if _, err := b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte("d")}); err != nil {
			t.Fatal(err)
		}
	}
	// The dropped frames are still recoverable via Read (ring retained them).
	got, _ := b.Read(ctx, "s1", 0)
	if len(got) != 8 { // capacity
		t.Errorf("Read recovered %d frames, want capacity 8", len(got))
	}
}

func TestMemBrokerSettleClearsRetainedFrames(t *testing.T) {
	b := NewMemBroker(8)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		_, _ = b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte("x")})
	}
	if err := b.Settle(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Read(ctx, "s1", 0)
	if len(got) != 0 {
		t.Errorf("after Settle, Read = %d frames, want 0 (content is durable in messages)", len(got))
	}
	// With no subscribers, Settle reclaimed the entry entirely: the next run's
	// publish recreates it and offsets restart at 1. Safe because attach filters
	// catch-up by RunID — a run's offsets are only ever compared within itself.
	off, _ := b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte("y")})
	if off != 1 {
		t.Errorf("offset after Settle = %d, want 1 (reclaimed entry restarts the ring)", off)
	}
}

// A settled session with no subscribers must leave the streams map entirely —
// the reclamation half of the mem broker's lifecycle (the Redis broker TTLs
// settled streams instead).
func TestMemBrokerSettleReclaimsIdleStream(t *testing.T) {
	b := NewMemBroker(8)
	ctx := context.Background()
	mb := b.(*memBroker)
	if _, err := b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	mb.mu.Lock()
	_, ok := mb.streams["s1"]
	mb.mu.Unlock()
	if !ok {
		t.Fatal("stream should exist before settle")
	}
	if err := b.Settle(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	mb.mu.Lock()
	_, ok = mb.streams["s1"]
	mb.mu.Unlock()
	if ok {
		t.Error("settled stream with no subscribers should be reclaimed")
	}
	// An unknown-session settle is a no-op, not an error.
	if err := b.Settle(ctx, "never-existed"); err != nil {
		t.Errorf("settle unknown session: %v", err)
	}
}

// A stream settled WITH a subscriber is kept (so the attached client still
// receives the run's terminal frame) and reclaimed when that subscriber
// detaches — otherwise every settled session would linger forever, which is
// the common flow (the submitter is attached at settle time).
func TestMemBrokerSettleKeepsEntryUntilLastUnsub(t *testing.T) {
	b := NewMemBroker(8)
	ctx := context.Background()
	mb := b.(*memBroker)
	ch, unsub := b.Subscribe("s1", 8)
	defer unsub()

	if _, err := b.Publish(ctx, "s1", StreamEvent{Kind: "text", Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	mb.mu.Lock()
	ls, ok := mb.streams["s1"]
	mb.mu.Unlock()
	if !ok || len(ls.frames) != 0 || len(ls.subs) != 1 {
		t.Fatalf("after settle with subscriber: exists=%v frames=%d subs=%d, want kept, empty frames, 1 sub",
			ok, len(ls.frames), len(ls.subs))
	}

	// The frame published while subscribed is drained (legitimately delivered).
	select {
	case <-ch:
	default:
		t.Error("subscribed client did not receive the live frame")
	}
	unsub()
	mb.mu.Lock()
	_, ok = mb.streams["s1"]
	mb.mu.Unlock()
	if ok {
		t.Error("entry should be reclaimed when the last subscriber of a settled stream detaches")
	}
}

func TestMemBrokerSeparateSessionsIsolated(t *testing.T) {
	b := NewMemBroker(8)
	ctx := context.Background()
	_, _ = b.Publish(ctx, "a", StreamEvent{Kind: "text", Payload: []byte("A")})
	_, _ = b.Publish(ctx, "b", StreamEvent{Kind: "text", Payload: []byte("B")})
	ga, _ := b.Read(ctx, "a", 0)
	gb, _ := b.Read(ctx, "b", 0)
	if len(ga) != 1 || string(ga[0].Payload) != "A" {
		t.Errorf("session a = %+v", ga)
	}
	if len(gb) != 1 || string(gb[0].Payload) != "B" {
		t.Errorf("session b = %+v", gb)
	}
}
