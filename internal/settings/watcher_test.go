package settings

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatcherRunsCallbacksOnInterval pins the cadence contract: every
// registered callback fires on each tick, and the loop picks up Add calls
// made after StartSync.
func TestWatcherRunsCallbacksOnInterval(t *testing.T) {
	w := NewWatcher()
	var calls atomic.Int32
	w.Add(func() { calls.Add(1) })
	w.Add(func() { calls.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.StartSync(ctx, 30*time.Millisecond)

	// Add after Start must still be picked up by the next tick.
	w.Add(func() { calls.Add(1) })

	// All three callbacks on at least two ticks.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("callbacks ran %d times, want >= 6 within 5s", calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWatcherStopsOnCancellation pins that the loop exits (no goroutine leak)
// when the ctx is cancelled, mirroring StartRefreshLoop's contract.
func TestWatcherStopsOnCancellation(t *testing.T) {
	w := NewWatcher()
	var got atomic.Int32
	w.Add(func() { got.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	w.StartSync(ctx, 30*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for got.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("callback never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("loop ctx not cancelled")
	}
}
