package builtin

import (
	"context"
	"sync"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// TestProgressUIToolStreamsFrames verifies ui_progress: with the loop's pusher
// in the ctx it pushes one spec per stage (live updates), and the final Result
// carries the settled 100% spec (durable for a reload).
func TestProgressUIToolStreamsFrames(t *testing.T) {
	var mu sync.Mutex
	pushed := 0
	ctx := toolruntime.ContextWithGenerativeUI(context.Background(), func(spec *provider.GenerativeUISpec) {
		mu.Lock()
		defer mu.Unlock()
		pushed++
		if spec == nil || len(spec.Root) != 1 {
			t.Errorf("pushed spec malformed: %+v", spec)
		}
	})

	res, err := NewProgressUI().Call(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pushed != len(progressStages) {
		t.Errorf("pusher invoked %d times, want %d stages", pushed, len(progressStages))
	}
	if res.GenerativeUI == nil {
		t.Fatal("final Result carries no GenerativeUI spec")
	}
	card := res.GenerativeUI.Root[0]
	if card.Props["percent"] != 100 {
		t.Errorf("final percent = %v, want 100", card.Props["percent"])
	}
	if card.Props["stage"] != "done" {
		t.Errorf("final stage = %v, want done", card.Props["stage"])
	}

	// Without the pusher (direct registry call) the tool still returns the
	// settled spec — the durable path never depends on the live one.
	res2, err := NewProgressUI().Call(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.GenerativeUI == nil {
		t.Error("settled spec missing without the pusher")
	}
}
