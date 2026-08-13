package inbound

import (
	"context"
	"errors"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
	"nowhere-agent/internal/toolruntime"
)

// stubProvider finishes a run with a single text delta — enough to prove the
// dispatcher's submission reaches the shared registry and settles done.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	ch <- provider.Event{Type: provider.EventMessageStart}
	ch <- provider.Event{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}}
	ch <- provider.Event{Type: provider.EventBlockDelta, Index: 0, Delta: "done"}
	ch <- provider.Event{Type: provider.EventBlockStop, Index: 0}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: provider.StopEndTurn}
	close(ch)
	return ch, nil
}

// memEnv wires a dispatcher over in-memory session state (no DB): runtime +
// registry + a loop builder over the stub provider.
func memEnv(t *testing.T) (*Dispatcher, *session.Runtime, *session.RunRegistry, *session.MemStore) {
	t.Helper()
	store := session.NewMemStore()
	rt := session.NewRuntime(store).WithBus(session.NewMemBus())
	rg := session.NewRunRegistry(rt)
	d := NewDispatcher(nil, rt, rg, nil, nil,
		func(ctx context.Context, userID, teamID, system, model string) (*agent.Loop, error) {
			return agent.New(stubProvider{}, toolruntime.NewRegistry(), agent.Config{Model: "m", MaxTokens: 100}), nil
		}, func() string { return "base" }, nil)
	d.SetClock(func() time.Time { return time.Now().UTC() })
	return d, rt, rg, store
}

func TestDispatchDisabled(t *testing.T) {
	d, _, _, _ := memEnv(t)
	wh := testWebhook("u")
	wh.Enabled = false
	if _, _, err := d.Dispatch(context.Background(), wh, "hello", nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("dispatch disabled: %v", err)
	}
}

func TestDispatchRunsToCompletion(t *testing.T) {
	d, rt, _, _ := memEnv(t)
	wh := testWebhook("u")

	runID, sessID, err := d.Dispatch(context.Background(), wh, "hello", map[string]any{"ticket": "123"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if runID == "" || sessID == "" {
		t.Fatalf("dispatch returned empty ids: run=%q session=%q", runID, sessID)
	}

	// The run settles done through the shared registry (async worker).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, active, _ := rt.ActiveRun(context.Background(), sessID); !active {
			runs, _ := rt.RunsForSession(context.Background(), sessID)
			if len(runs) > 0 {
				if runs[len(runs)-1].Status != session.RunDone {
					t.Fatalf("final status = %v, want done", runs[len(runs)-1].Status)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("run did not settle")
}

func TestDispatchRejectsPendingInteraction(t *testing.T) {
	d, rt, _, store := memEnv(t)
	// Park a pending approval on the session, then dispatch into it via a
	// target session id. The dispatcher must refuse to bury the pending
	// decision.
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApproval(context.Background(), session.Approval{SessionID: sess.ID, ToolName: "edit_file"}); err != nil {
		t.Fatal(err)
	}

	wh := testWebhook("u")
	wh.TargetSessionID = sess.ID
	if _, _, err := d.Dispatch(context.Background(), wh, "hello", nil); !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("dispatch with pending interaction: %v", err)
	}
}

func TestDispatchRejectsForeignTargetSession(t *testing.T) {
	d, rt, _, _ := memEnv(t)
	// A session owned by user "victim" — the webhook belongs to "u".
	victimSess, err := rt.CreateSession(context.Background(), "victim", "private")
	if err != nil {
		t.Fatal(err)
	}
	wh := testWebhook("u")
	wh.TargetSessionID = victimSess.ID
	if _, _, err := d.Dispatch(context.Background(), wh, "hello", nil); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("dispatch into foreign session: %v, want ErrNotOwner", err)
	}
}

// ScopeResolver stub used by the agent-def resolution path.
type stubScopes struct{}

func (stubScopes) AccessibleScopes(ctx context.Context, userID string) ([]identity.ScopeRef, error) {
	return []identity.ScopeRef{identity.UserScope(userID), identity.SystemScope()}, nil
}

func TestDispatchAgentDefPromptSource(t *testing.T) {
	d, _, _, _ := memEnv(t)
	d.identity = stubScopes{}
	// No def resolver wired: resolvePrompt logs the failure and falls back to
	// the fixed system prompt — dispatch must still succeed.
	wh := testWebhook("u")
	wh.AgentDef = "missing-def"
	wh.SystemPrompt = ""

	if _, _, err := d.Dispatch(context.Background(), wh, "hello", nil); err != nil {
		t.Fatalf("dispatch with unresolvable def: %v", err)
	}
}
