package session

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisEventBusFanout verifies a lifecycle event published on one instance's
// bus is delivered to a subscriber on another instance sharing the same Redis.
func TestRedisEventBusFanout(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	pubCli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	subCli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer pubCli.Close()
	defer subCli.Close()

	// Two "instances" sharing the durable store would each run their own bus over
	// the same Redis; here one publishes and the other subscribes.
	pub := NewRedisEventBusFromClient(pubCli)
	sub := NewRedisEventBusFromClient(subCli)

	ch, unsub := sub.Subscribe("sess1", 8)
	defer unsub()
	time.Sleep(100 * time.Millisecond) // let the subscription establish (pub/sub has no retention)

	pub.Publish("sess1", Event{RunID: "r1", SessionID: "sess1", Kind: "done", Offset: 3})

	select {
	case e := <-ch:
		if e.Kind != "done" || e.RunID != "r1" || e.Offset != 3 {
			t.Errorf("received %+v, want a done event for r1 at offset 3", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive the event published on the other instance")
	}
}

// TestRedisEventBusScopedToSession verifies events are delivered only to
// subscribers of the same session channel.
func TestRedisEventBusScopedToSession(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cli.Close()
	bus := NewRedisEventBusFromClient(cli)

	ch, unsub := bus.Subscribe("sessA", 8)
	defer unsub()
	time.Sleep(100 * time.Millisecond)

	bus.Publish("sessB", Event{RunID: "r9", SessionID: "sessB", Kind: "done"})

	select {
	case e := <-ch:
		t.Errorf("received cross-session event %+v, want none", e)
	case <-time.After(300 * time.Millisecond):
		// expected: no delivery to a different session's subscriber
	}
}
