package session

import (
	"context"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime/builtin"
)

// clientToolCallProvider drives a run that calls one client-side tool and stops,
// so the loop suspends on the interaction gate (client_tool kind).
type clientToolCallProvider struct{ toolName, callID string }

func (p *clientToolCallProvider) Name() string { return "cttool" }

func (p *clientToolCallProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: p.callID, ToolName: p.toolName, ToolInput: map[string]any{}}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: `{}`}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopToolUse}
	close(ch)
	return ch, nil
}

// TestInteractionRowPersistedBeforeFramePublished pins the resume-race fix: the
// durable Interaction row MUST be committed before the data-interaction frame is
// published to the broker. Otherwise a fast client (an instant client-tool
// auto-run) receives the frame, POSTs its verdict with the interaction id, and
// 404s because the row hasn't been written yet — the bug a user hit testing an
// instant client tool ("sometimes 404, wait and it works").
//
// We subscribe to the broker and, the moment the interrupt frame arrives, assert
// the session already has a pending interaction. Under the pre-fix ordering
// (persist after Run() returned, i.e. after the frame was already published)
// this observes ok=false and fails.
func TestInteractionRowPersistedBeforeFramePublished(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	rg := NewRunRegistry(rt)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}

	loop := registryLoop(&clientToolCallProvider{toolName: "sleep", callID: "tc-1"})
	loop.RegisterTool(builtin.NewClientTool("sleep",
		"sleep in the browser",
		map[string]any{"type": "object"},
		map[string]any{"type": "object"},
	))

	// Subscribe BEFORE submitting so the interrupt frame is not missed.
	ch, unsub := rt.Broker().Subscribe(sess.ID, 16)
	defer unsub()

	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-ch:
			if agent.EventKind(e.Kind) != agent.KindInterrupt {
				continue // tool_use precedes the interrupt frame on the broker
			}
			// The frame is out; the row it references must already be resolvable.
			ap, ok, err := rg.PendingApprovalForSession(context.Background(), sess.ID)
			if err != nil || !ok {
				t.Fatalf("interaction row must exist when the frame is published (ok=%v err=%v)", ok, err)
			}
			if ap.Kind != KindClientTool {
				t.Errorf("pending kind = %q, want client_tool", ap.Kind)
			}
			return
		case <-deadline:
			t.Fatal("no interrupt frame observed on the broker")
		}
	}
}
