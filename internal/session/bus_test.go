package session

import (
	"testing"
	"time"
)

func TestMemBusFanoutToAllSubscribers(t *testing.T) {
	bus := NewMemBus()
	const subs = 3
	chans := make([]<-chan Event, subs)
	for i := range chans {
		ch, unsub := bus.Subscribe("s1", 8)
		defer unsub()
		chans[i] = ch
	}

	bus.Publish("s1", Event{SessionID: "s1", Kind: "text", Offset: 1})

	for i, ch := range chans {
		select {
		case e := <-ch:
			if e.Offset != 1 || e.Kind != "text" {
				t.Errorf("subscriber %d got %+v", i, e)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d did not receive the event", i)
		}
	}
}

func TestMemBusScopedToSession(t *testing.T) {
	bus := NewMemBus()
	chA, unsubA := bus.Subscribe("a", 8)
	defer unsubA()
	chB, unsubB := bus.Subscribe("b", 8)
	defer unsubB()

	bus.Publish("a", Event{SessionID: "a", Offset: 1})

	select {
	case <-chA:
	case <-time.After(time.Second):
		t.Error("subscriber on session a did not receive its event")
	}
	select {
	case e := <-chB:
		t.Errorf("subscriber on session b wrongly received event %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMemBusSlowConsumerDrops(t *testing.T) {
	bus := NewMemBus()
	ch, unsub := bus.Subscribe("s1", 1) // buffer of 1
	defer unsub()

	// Publish more than the buffer holds without draining: the run must not block.
	for i := 1; i <= 5; i++ {
		bus.Publish("s1", Event{SessionID: "s1", Offset: i})
	}

	// The first event landed; the rest were dropped for the slow consumer.
	select {
	case e := <-ch:
		if e.Offset != 1 {
			t.Errorf("got offset %d, want 1", e.Offset)
		}
	case <-time.After(time.Second):
		t.Error("expected the first event to be buffered")
	}
	select {
	case e := <-ch:
		t.Errorf("expected drop, got extra event %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMemBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewMemBus()
	ch, unsub := bus.Subscribe("s1", 8)

	unsub()
	// Publishing after unsubscribe must not be delivered (and must not panic: the
	// channel is left open precisely so an in-flight Publish never hits a closed
	// channel).
	bus.Publish("s1", Event{SessionID: "s1", Offset: 1})
	select {
	case e := <-ch:
		t.Errorf("event delivered after unsubscribe: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}
