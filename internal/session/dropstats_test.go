package session

import (
	"context"
	"testing"
)

// TestMemBusDroppedTotalCountsSlowConsumerDrops pins the delivery-health
// counter: drops for slow subscribers were previously silent.
func TestMemBusDroppedTotalCountsSlowConsumerDrops(t *testing.T) {
	b := NewMemBus()
	ds, ok := b.(DropStats)
	if !ok {
		t.Fatal("memBus does not implement DropStats")
	}
	// Buffer of 1, never drained: the first event fills it, the rest drop.
	_, unsub := b.Subscribe("s", 1)
	defer unsub()
	for i := 0; i < 5; i++ {
		b.Publish("s", Event{RunID: "r"})
	}
	if got := ds.DroppedTotal(); got != 4 {
		t.Errorf("dropped = %d, want 4", got)
	}
}

func TestMemBrokerDroppedTotalCountsSlowConsumerDrops(t *testing.T) {
	b := NewMemBroker(0)
	ds, ok := b.(DropStats)
	if !ok {
		t.Fatal("memBroker does not implement DropStats")
	}
	_, unsub := b.Subscribe("s", 1)
	defer unsub()
	for i := 0; i < 5; i++ {
		if _, err := b.Publish(context.Background(), "s", StreamEvent{RunID: "r", Kind: "text"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := ds.DroppedTotal(); got != 4 {
		t.Errorf("dropped = %d, want 4", got)
	}
}

// TestMemBrokerNoDropsForHealthyConsumer guards against counting normal
// deliveries as drops.
func TestMemBrokerNoDropsForHealthyConsumer(t *testing.T) {
	b := NewMemBroker(0)
	ch, unsub := b.Subscribe("s", 16)
	defer unsub()
	for i := 0; i < 3; i++ {
		if _, err := b.Publish(context.Background(), "s", StreamEvent{RunID: "r", Kind: "text"}); err != nil {
			t.Fatal(err)
		}
		<-ch
	}
	if got := b.(DropStats).DroppedTotal(); got != 0 {
		t.Errorf("dropped = %d, want 0 for a drained subscriber", got)
	}
}
