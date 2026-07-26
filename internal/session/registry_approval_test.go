package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// approvalScriptProvider emits a fixed script of provider event batches, one
// per Stream call (like agent's scriptProvider, re-declared for this package).
type approvalScriptProvider struct {
	mu     sync.Mutex
	script [][]provider.Event
	calls  int
}

func (p *approvalScriptProvider) Name() string { return "approval-script" }

func (p *approvalScriptProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls >= len(p.script) {
		return nil, errors.New("no more scripted responses")
	}
	evs := p.script[p.calls]
	p.calls++
	ch := make(chan provider.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func textTurn(text string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockText}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: text},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop, Usage: &provider.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

func toolUseTurn(id, name, args string) []provider.Event {
	return []provider.Event{
		{Type: provider.EventMessageStart},
		{Type: provider.EventBlockStart, Index: 0, Block: &provider.Block{Type: provider.BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: map[string]any{}}},
		{Type: provider.EventBlockDelta, Index: 0, Delta: args},
		{Type: provider.EventBlockStop, Index: 0},
		{Type: provider.EventMessageStop},
	}
}

// gatedDangerTool records whether it ran.
type gatedDangerTool struct{ ran *bool }

func (g gatedDangerTool) Name() string           { return "danger" }
func (g gatedDangerTool) Description() string    { return "gated" }
func (g gatedDangerTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (g gatedDangerTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }
func (g gatedDangerTool) Timeout() time.Duration { return time.Second }
func (g gatedDangerTool) Call(context.Context, map[string]any) (toolruntime.Result, error) {
	*g.ran = true
	return toolruntime.Result{Content: "danger done"}, nil
}

func gateAllPermission(toolruntime.Tool) (bool, string) {
	return true, agent.ApprovalReasonPrefix + "ask"
}

// newApprovalRegistry builds a registry wired for approval: gated tool, message
// store, and a permission that gates everything.
func newApprovalRegistry(t *testing.T, prov provider.Adapter, ran *bool) (*Runtime, *RunRegistry, Session, *MemMessageStore) {
	t.Helper()
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	rg := NewRunRegistry(rt, rt.Bus())
	ms := NewMemMessageStore()
	rg.WithMessageStore(ms)
	sess, err := rt.CreateSession(context.Background(), "u", "t")
	if err != nil {
		t.Fatal(err)
	}
	reg := toolruntime.NewRegistry()
	reg.Register(gatedDangerTool{ran: ran})
	loop := agent.New(prov, reg, agent.Config{Model: "m", MaxTokens: 100, Permission: gateAllPermission})
	if _, err := rg.Submit(context.Background(), sess.ID, RunWork{Loop: loop}); err != nil {
		t.Fatal(err)
	}
	return rt, rg, sess, ms
}

// waitStatus polls until the session's run reaches the wanted status.
func waitStatus(t *testing.T, rt *Runtime, sessionID string, want RunStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := rt.RunsForSession(context.Background(), sessionID)
		if len(runs) > 0 && runs[len(runs)-1].Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	runs, _ := rt.RunsForSession(context.Background(), sessionID)
	t.Fatalf("run never reached %v (now %v)", want, runs[len(runs)-1].Status)
}

// TestRunSuspendsThenResumeApproved pins the full O2 loop: a gated call parks
// the run in waiting_approval (releasing the lock) without executing; Resume
// with approve re-acquires the lock, executes the call, and runs to done.
func TestRunSuspendsThenResumeApproved(t *testing.T) {
	ran := false
	prov := &approvalScriptProvider{script: [][]provider.Event{
		toolUseTurn("tu1", "danger", `{"path":"/etc"}`),
		textTurn("all done"),
	}}
	rt, rg, sess, _ := newApprovalRegistry(t, prov, &ran)

	// The run parks in waiting_approval without running the tool.
	waitStatus(t, rt, sess.ID, RunWaitingApproval)
	if ran {
		t.Fatal("gated tool executed before approval")
	}

	// The lock was released: a fresh run could start (parked is not holding it).
	if _, active, _ := rt.ActiveRun(context.Background(), sess.ID); !active {
		t.Fatal("parked run should still read as Active (waiting_approval)")
	}

	// Find the pending approval and approve it.
	ap, ok, err := rt.store.PendingApprovalForRun(context.Background(), currentRunID(t, rt, sess.ID))
	if err != nil || !ok {
		t.Fatalf("no pending approval: ok=%v err=%v", ok, err)
	}
	if ap.ToolName != "danger" || ap.ToolCallID != "tu1" {
		t.Fatalf("approval = %+v", ap)
	}

	if _, err := rg.Resume(context.Background(), ap.ID, true, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitStatus(t, rt, sess.ID, RunDone)
	if !ran {
		t.Error("approved tool should have executed on resume")
	}
}

// TestResumeRejectedSkipsExecution: a rejected approval resumes the run, injects
// a denial, and does NOT execute the tool.
func TestResumeRejectedSkipsExecution(t *testing.T) {
	ran := false
	prov := &approvalScriptProvider{script: [][]provider.Event{
		toolUseTurn("tu1", "danger", `{}`),
		textTurn("ok, I won't"),
	}}
	rt, rg, sess, _ := newApprovalRegistry(t, prov, &ran)
	waitStatus(t, rt, sess.ID, RunWaitingApproval)

	ap, _, _ := rt.store.PendingApprovalForRun(context.Background(), currentRunID(t, rt, sess.ID))
	if _, err := rg.Resume(context.Background(), ap.ID, false, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitStatus(t, rt, sess.ID, RunDone)
	if ran {
		t.Error("rejected tool must not execute")
	}
}

// TestResumeUnknownApproval: an unknown or already-decided approval errors.
func TestResumeUnknownApproval(t *testing.T) {
	rt := NewRuntime(NewMemStore()).WithBus(NewMemBus())
	rg := NewRunRegistry(rt, rt.Bus())
	if _, err := rg.Resume(context.Background(), "no-such-id", true, nil); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("Resume unknown: %v", err)
	}
}

// TestResumeAfterRestartRebuildsLoop pins cross-restart resume: the parked map
// is empty (process restarted), so Resume rebuilds the loop via the LoopSource
// and still executes the approved call.
func TestResumeAfterRestartRebuildsLoop(t *testing.T) {
	ran := false
	// Park a run in one registry (the "old process").
	prov1 := &approvalScriptProvider{script: [][]provider.Event{toolUseTurn("tu1", "danger", `{}`)}}
	rt, rg1, sess, _ := newApprovalRegistry(t, prov1, &ran)
	waitStatus(t, rt, sess.ID, RunWaitingApproval)
	_ = rg1 // old registry's parked map now holds the work; a restart drops it.

	// New registry over the SAME runtime/store (the "new process"): empty parked
	// map, but a LoopSource that rebuilds the loop.
	ran2 := false
	rg2 := NewRunRegistry(rt, rt.Bus()).WithMessageStore(NewMemMessageStore())
	// Rebuild history needs the durable messages; reuse the first store's data is
	// not required for execution, only for context. Use a fresh loop source.
	prov2 := &approvalScriptProvider{script: [][]provider.Event{textTurn("resumed after restart")}}
	rg2.WithLoopSource(func(ctx context.Context, sessionID string) (*agent.Loop, error) {
		reg := toolruntime.NewRegistry()
		reg.Register(gatedDangerTool{ran: &ran2})
		return agent.New(prov2, reg, agent.Config{Model: "m", MaxTokens: 100, Permission: gateAllPermission}), nil
	})

	ap, _, _ := rt.store.PendingApprovalForRun(context.Background(), currentRunID(t, rt, sess.ID))
	if _, err := rg2.Resume(context.Background(), ap.ID, true, nil); err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}
	waitStatus(t, rt, sess.ID, RunDone)
	if !ran2 {
		t.Error("approved tool should execute via the rebuilt loop after restart")
	}
}

// TestPendingApprovalForSession pins the reload-restore lookup: a parked run's
// pending approval is reachable by session (so /history can echo it to a
// refreshed client), and disappears once decided.
func TestPendingApprovalForSession(t *testing.T) {
	ran := false
	prov := &approvalScriptProvider{script: [][]provider.Event{
		toolUseTurn("tu1", "danger", `{"q":"?"}`),
		textTurn("done"),
	}}
	rt, rg, sess, _ := newApprovalRegistry(t, prov, &ran)
	waitStatus(t, rt, sess.ID, RunWaitingApproval)

	ap, ok, err := rg.PendingApprovalForSession(context.Background(), sess.ID)
	if err != nil || !ok {
		t.Fatalf("pending for session: ok=%v err=%v", ok, err)
	}
	if ap.ToolCallID != "tu1" || ap.ToolName != "danger" || ap.Status != ApprovalPending {
		t.Fatalf("approval = %+v", ap)
	}
	if len(ap.ToolInput) == 0 {
		t.Error("pending approval should carry the gated call's input for re-render")
	}

	// Once decided, the session has no outstanding interaction.
	if _, err := rg.Resume(context.Background(), ap.ID, true, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitStatus(t, rt, sess.ID, RunDone)
	if _, ok, err := rg.PendingApprovalForSession(context.Background(), sess.ID); err != nil || ok {
		t.Errorf("after resume: ok=%v err=%v, want none", ok, err)
	}
}

func currentRunID(t *testing.T, rt *Runtime, sessionID string) string {
	t.Helper()
	runs, _ := rt.RunsForSession(context.Background(), sessionID)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	return runs[len(runs)-1].ID
}
